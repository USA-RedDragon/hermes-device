// Package acpbridge is `talaria acp`. It speaks ACP (newline-delimited
// JSON-RPC 2.0) with an editor on stdio and copies it to the hub's ACP leg,
// with the device-side behavior of docs/protocol.md section 6.3: file links
// are inlined, sessions are bound to the editor's directory, and a lost leg
// is reconnected and its sessions loaded again without replaying their
// history to the editor.
package acpbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

// ACP methods the bridge handles.
const (
	methodInitialize = "initialize"
	methodNew        = "session/new"
	methodLoad       = "session/load"
	methodResume     = "session/resume"
	methodFork       = "session/fork"
	methodPrompt     = "session/prompt"
	methodUpdate     = "session/update"
	methodClose      = "session/close"
	methodCancel     = "session/cancel"
)

// JSON-RPC error codes the bridge answers with.
const (
	CodeParse         = -32700
	CodeInvalidReq    = -32600
	CodeInvalidParams = -32602
	CodeInternal      = -32603
)

// MsgLost is the error message of editor requests that were in flight when
// the ACP leg dropped.
const MsgLost = "talaria: connection to Hermes lost; retry"

// IDPrefix starts the ids of requests the bridge sends itself.
const IDPrefix = "hermes-device-"

// keyMeta is the ACP extension member.
const keyMeta = "_meta"

// keyJSONRPC is the JSON-RPC version member.
const keyJSONRPC = "jsonrpc"

// jsonNull is the JSON encoding of null.
const jsonNull = "null"

const (
	stableAfter = 30 * time.Second
	callTimeout = 60 * time.Second
	bindTimeout = 30 * time.Second
	maxMessage  = 64 << 20
	// maxForward keeps editor messages under the 50 MiB line limit of
	// hermes acp, which acp-host enforces at 48 MiB.
	maxForward = 48 << 20
)

// ErrUnsupportedVersion is returned when the hub or acp-host closes the leg
// with protocol.CloseUnsupportedVersion.
var ErrUnsupportedVersion = errors.New("the server does not support this talaria version; upgrade talaria")

var (
	errLost    = errors.New("ACP leg lost")
	errTimeout = errors.New("no answer from Hermes")
)

// Hub is the part of the hub API the bridge uses. *hubapi.Client
// implements it.
type Hub interface {
	DialLeg(ctx context.Context, path string) (*websocket.Conn, error)
	Bind(ctx context.Context, sessionID, dir string) (*hubapi.Binding, error)
}

// Options configure a Bridge.
type Options struct {
	Hub Hub
	// In and Out are the editor's side: requests in, JSON-RPC out. Nothing
	// else is ever written to Out.
	In           io.Reader
	Out          io.Writer
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
	PingInterval time.Duration
	// ReadyTimeout is how long an editor request waits for the ACP leg
	// before it is answered with MsgLost. It defaults to 30s.
	ReadyTimeout time.Duration
	Logger       *slog.Logger
}

type object = map[string]json.RawMessage

// pendingReq is an editor request sent to the agent and not answered yet.
type pendingReq struct {
	id      json.RawMessage
	method  string
	params  json.RawMessage
	cwd     string
	session string
	gen     uint64
}

// agentReq is an agent request passed to the editor under a bridge id.
type agentReq struct {
	id  json.RawMessage
	gen uint64
}

// ownCall is a request the bridge sent itself.
type ownCall struct {
	ch      chan object
	session string
}

type openSession struct {
	id, cwd string
}

// Bridge copies ACP between an editor and the hub.
type Bridge struct {
	opts  Options
	log   *slog.Logger
	outMu sync.Mutex
	fatal chan error

	mu      sync.Mutex
	gen     uint64
	conn    *websocket.Conn
	ready   bool
	readyCh chan struct{}
	nextID  uint64
	// init holds the params of the editor's last successful initialize.
	init json.RawMessage
	// sessions are the sessions the editor opened, in the order it opened
	// them.
	sessions  []openSession
	pending   map[string]pendingReq
	own       map[string]ownCall
	agentReqs map[string]agentReq
	replaying map[string]bool
}

// New returns a bridge for opts.
func New(opts Options) *Bridge {
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = time.Second
	}
	if opts.MaxBackoff < opts.MinBackoff {
		opts.MaxBackoff = max(opts.MinBackoff, 30*time.Second)
	}
	if opts.PingInterval <= 0 {
		opts.PingInterval = protocol.PingInterval
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = 30 * time.Second
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Bridge{
		opts: opts, log: log, fatal: make(chan error, 1), readyCh: make(chan struct{}),
		pending: map[string]pendingReq{}, own: map[string]ownCall{},
		agentReqs: map[string]agentReq{}, replaying: map[string]bool{},
	}
}

// Run bridges until the editor closes its input (nil), ctx ends (nil), or
// the hub refuses the device (hubapi.ErrUnauthorized or
// ErrUnsupportedVersion).
func (b *Bridge) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go readEditor(ctx, b.opts.In, lines, readErr)
	connErr := make(chan error, 1)
	go func() { connErr <- b.connect(ctx) }()
	editorErr := make(chan error, 1)
	go func() { editorErr <- b.editorLoop(ctx, lines, readErr) }()

	var err error
	editorDone, connDone := false, false
	select {
	case err = <-editorErr:
		editorDone = true
	case err = <-connErr:
		connDone = true
	case err = <-b.fatal:
	}
	cancel()
	if !editorDone {
		if eerr := <-editorErr; eerr != nil {
			b.log.Debug("Editor loop ended after the bridge stopped", "error", eerr)
		}
	}
	if !connDone {
		if cerr := <-connErr; err == nil {
			err = cerr
		}
	}
	if err == nil {
		select {
		case err = <-b.fatal:
		default:
		}
	}
	return err
}

func readEditor(ctx context.Context, in io.Reader, lines chan<- []byte, errc chan<- error) {
	br := bufio.NewReaderSize(in, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			errc <- err
			return
		}
	}
}

func (b *Bridge) editorLoop(ctx context.Context, lines <-chan []byte, readErr <-chan error) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case line := <-lines:
			if err := b.fromEditor(ctx, line); err != nil {
				return err
			}
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				b.log.Info("Editor closed its input")
				return nil
			}
			return err
		}
	}
}

// fromEditor handles one editor message. It returns an error only when the
// bridge must stop.
func (b *Bridge) fromEditor(ctx context.Context, data []byte) error {
	var m object
	if err := json.Unmarshal(data, &m); err != nil {
		if json.Valid(data) {
			b.writeEditor(errorMsg(nil, CodeInvalidReq, "talaria: a message must be one JSON-RPC object"))
		} else {
			b.writeEditor(errorMsg(nil, CodeParse, "talaria: parse error"))
		}
		return nil
	}
	method := str(m["method"])
	_, withID := m["id"]
	if method == "" {
		if withID {
			b.answerAgent(ctx, m)
		}
		return nil
	}
	stripMeta(m)
	var cwd, session string
	switch method {
	case methodPrompt:
		b.inlinePrompt(m)
	case methodClose:
		b.closeSession(ctx, m, withID)
		return nil
	case methodNew, methodFork, methodLoad, methodResume:
		p := params(m)
		cwd = str(p["cwd"])
		session = str(p["sessionId"])
		if !filepath.IsAbs(cwd) {
			if withID {
				b.writeEditor(errorMsg(m["id"], CodeInvalidParams, "talaria: cwd must be an absolute path"))
			}
			return nil
		}
		cwd = filepath.Clean(cwd)
		if method == methodLoad || method == methodResume {
			if err := b.bind(ctx, session, cwd); err != nil {
				if withID {
					b.writeEditor(bindError(m["id"], cwd, err))
				}
				if errors.Is(err, hubapi.ErrUnauthorized) {
					return err
				}
				return nil
			}
		}
	}
	return b.forward(ctx, m, pendingReq{id: m["id"], method: method, params: m["params"], cwd: cwd, session: session}, withID)
}

// stripMeta removes a top-level _meta member from the params of an editor
// request or notification. The hub refuses it (docs/protocol.md 6.1),
// because the ACP SDK in Hermes merges it into the handler's arguments.
func stripMeta(m object) {
	p := params(m)
	if _, ok := p[keyMeta]; !ok {
		return
	}
	delete(p, keyMeta)
	m["params"] = mustJSON(p)
}

// forward sends m on the current connection once it is ready, under a
// request id of the bridge's own. A request that finds no ready
// connection within ReadyTimeout is answered with MsgLost.
func (b *Bridge) forward(ctx context.Context, m object, p pendingReq, withID bool) error {
	if data, err := json.Marshal(m); err != nil || len(data) > maxForward {
		if withID {
			b.writeEditor(errorMsg(p.id, CodeInvalidReq, "talaria: message too large"))
		}
		return nil
	}
	wctx, cancel := context.WithTimeout(ctx, b.opts.ReadyTimeout)
	defer cancel()
	for {
		conn, gen, err := b.waitReady(wctx)
		if err != nil {
			if ctx.Err() == nil && withID {
				b.writeEditor(errorMsg(p.id, CodeInternal, MsgLost))
			}
			return nil
		}
		b.mu.Lock()
		if !b.ready || b.gen != gen {
			b.mu.Unlock()
			continue
		}
		var own string
		if withID {
			b.nextID++
			own = fmt.Sprintf("%s%d", IDPrefix, b.nextID)
			p.gen = gen
			b.pending[own] = p
			m["id"] = mustJSON(own)
		}
		b.mu.Unlock()
		data, err := json.Marshal(m)
		if err == nil {
			err = conn.Write(ctx, websocket.MessageText, data)
		}
		if err != nil && withID {
			b.mu.Lock()
			_, ok := b.pending[own]
			delete(b.pending, own)
			b.mu.Unlock()
			if ok {
				b.writeEditor(errorMsg(p.id, CodeInternal, MsgLost))
			}
		}
		return nil
	}
}

func (b *Bridge) waitReady(ctx context.Context) (*websocket.Conn, uint64, error) {
	for {
		b.mu.Lock()
		if b.ready {
			conn, gen := b.conn, b.gen
			b.mu.Unlock()
			return conn, gen, nil
		}
		ch := b.readyCh
		b.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
}

// answerAgent passes the editor's answer to an agent request back under
// the agent's id. Answers to requests of an older connection are dropped.
func (b *Bridge) answerAgent(ctx context.Context, m object) {
	key := str(m["id"])
	b.mu.Lock()
	ar, ok := b.agentReqs[key]
	if ok {
		delete(b.agentReqs, key)
	}
	current := ok && b.conn != nil && ar.gen == b.gen
	conn := b.conn
	b.mu.Unlock()
	if !current {
		b.log.Debug("Dropped an editor answer to a request of a lost connection", "id", key)
		return
	}
	m["id"] = ar.id
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = conn.Write(ctx, websocket.MessageText, data)
}

func (b *Bridge) bind(ctx context.Context, session, dir string) error {
	if session == "" {
		return errors.New("no sessionId")
	}
	ctx, cancel := context.WithTimeout(ctx, bindTimeout)
	defer cancel()
	_, err := b.opts.Hub.Bind(ctx, session, dir)
	if err != nil {
		b.log.Warn("Could not bind a session", "session", session, "dir", dir, "error", err)
	}
	return err
}

func bindError(id json.RawMessage, dir string, err error) []byte {
	return errorMsg(id, CodeInternal, fmt.Sprintf("talaria: could not bind the session to %s: %v", dir, err))
}

// connect keeps one ACP leg up, with backoff between attempts.
func (b *Bridge) connect(ctx context.Context) error {
	backoff := b.opts.MinBackoff
	for {
		conn, err := b.opts.Hub.DialLeg(ctx, hubapi.PathACPLeg)
		if err == nil {
			b.log.Info("ACP leg connected")
			started := time.Now()
			err = b.serve(ctx, conn)
			if time.Since(started) >= stableAfter {
				backoff = b.opts.MinBackoff
			}
		}
		if ctx.Err() != nil {
			return nil //nolint:nilerr // the caller stopped the bridge; the leg error is the shutdown itself
		}
		if ferr := fatalError(err); ferr != nil {
			return ferr
		}
		b.log.Warn("ACP leg lost; reconnecting", "error", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, b.opts.MaxBackoff)
	}
}

func fatalError(err error) error {
	if errors.Is(err, hubapi.ErrUnauthorized) {
		return hubapi.ErrUnauthorized
	}
	switch websocket.CloseStatus(err) { //nolint:exhaustive // close codes are an open set; only these two are fatal
	case protocol.CloseUnauthorized:
		return hubapi.ErrUnauthorized
	case protocol.CloseUnsupportedVersion:
		return ErrUnsupportedVersion
	}
	return nil
}

// serve runs one connection until it drops.
func (b *Bridge) serve(ctx context.Context, conn *websocket.Conn) error {
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxMessage)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	b.mu.Lock()
	b.gen++
	gen := b.gen
	b.conn = conn
	b.mu.Unlock()

	// The reader never waits on the processor, which may hold a response
	// while it binds, so pongs keep being read.
	msgs := newQueue()
	var readErr error
	go func() {
		for {
			_, data, err := conn.Read(cctx)
			if err != nil {
				readErr = err
				msgs.close()
				return
			}
			msgs.push(data)
		}
	}()
	procDone := make(chan struct{})
	go func() {
		defer close(procDone)
		for {
			data, ok := msgs.pop()
			if !ok {
				return
			}
			b.fromAgent(cctx, gen, data)
		}
	}()
	go b.ping(cctx, conn)

	err := b.replay(cctx, conn, procDone)
	if err == nil {
		b.mu.Lock()
		b.ready = true
		close(b.readyCh)
		b.mu.Unlock()
		<-procDone
		err = readErr
	} else {
		b.log.Warn("Could not restore the ACP sessions on the new connection", "error", err)
		cancel()
		<-procDone
		if errors.Is(err, errLost) {
			err = readErr
		}
	}
	b.teardown()
	return err
}

// replay restores the editor's state on a new connection: initialize with
// the editor's params, then session/load for every open session.
func (b *Bridge) replay(ctx context.Context, conn *websocket.Conn, procDone <-chan struct{}) error {
	b.mu.Lock()
	init := b.init
	sessions := append([]openSession(nil), b.sessions...)
	b.mu.Unlock()
	if init == nil {
		return nil
	}
	resp, err := b.call(ctx, conn, procDone, methodInitialize, init, "")
	if err != nil {
		return err
	}
	if e, ok := resp["error"]; ok {
		return fmt.Errorf("initialize failed: %s", e)
	}
	for _, s := range sessions {
		params, err := json.Marshal(map[string]any{"sessionId": s.id, "cwd": s.cwd, "mcpServers": []any{}})
		if err != nil {
			return err
		}
		resp, err := b.call(ctx, conn, procDone, methodLoad, params, s.id)
		if err != nil {
			return err
		}
		if e, ok := resp["error"]; ok {
			b.log.Warn("Could not load a session again; the editor must open it again", "session", s.id, "error", string(e))
			b.dropSession(s.id)
			continue
		}
		b.log.Info("Loaded a session again", "session", s.id)
	}
	return nil
}

// call sends a request of the bridge's own and waits for its response.
// For a session/load, the session's updates are dropped until the
// response.
func (b *Bridge) call(ctx context.Context, conn *websocket.Conn, procDone <-chan struct{}, method string, params json.RawMessage, session string) (object, error) {
	ch := make(chan object, 1)
	b.mu.Lock()
	b.nextID++
	id := fmt.Sprintf("%s%d", IDPrefix, b.nextID)
	rawID := mustJSON(id)
	b.own[id] = ownCall{ch: ch, session: session}
	if session != "" {
		b.replaying[session] = true
	}
	b.mu.Unlock()
	data, err := json.Marshal(object{keyJSONRPC: json.RawMessage(`"2.0"`), "id": rawID, "method": mustJSON(method), "params": params})
	if err != nil {
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return nil, errLost
	}
	timer := time.NewTimer(callTimeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		return resp, nil
	case <-procDone:
		return nil, errLost
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("%s: %w", method, errTimeout)
	}
}

// teardown ends a connection: every editor request still waiting gets
// MsgLost, and the state of the connection is forgotten.
func (b *Bridge) teardown() {
	b.mu.Lock()
	if b.ready {
		b.readyCh = make(chan struct{})
	}
	b.ready = false
	b.conn = nil
	lost := make([]json.RawMessage, 0, len(b.pending))
	for _, p := range b.pending {
		lost = append(lost, p.id)
	}
	clear(b.pending)
	clear(b.own)
	clear(b.agentReqs)
	clear(b.replaying)
	b.mu.Unlock()
	for _, id := range lost {
		b.writeEditor(errorMsg(id, CodeInternal, MsgLost))
	}
}

// fromAgent handles one message from the agent, in order.
func (b *Bridge) fromAgent(ctx context.Context, gen uint64, data []byte) {
	var m object
	if err := json.Unmarshal(data, &m); err != nil {
		b.log.Warn("Dropped a message from Hermes that is not a JSON object", "error", err)
		return
	}
	method := str(m["method"])
	withID := hasID(m)
	switch {
	case method != "" && withID:
		b.mu.Lock()
		b.nextID++
		id := fmt.Sprintf("%s%d", IDPrefix, b.nextID)
		b.agentReqs[id] = agentReq{id: m["id"], gen: gen}
		b.mu.Unlock()
		m["id"] = mustJSON(id)
		b.writeEditor(mustJSON(m))
	case method != "":
		if method == methodUpdate {
			session := str(params(m)["sessionId"])
			b.mu.Lock()
			drop := b.replaying[session]
			b.mu.Unlock()
			if drop {
				return
			}
		}
		b.writeEditor(mustJSON(m))
	case withID:
		key := str(m["id"])
		b.mu.Lock()
		if oc, ok := b.own[key]; ok {
			delete(b.own, key)
			if oc.session != "" {
				delete(b.replaying, oc.session)
			}
			b.mu.Unlock()
			oc.ch <- m
			return
		}
		p, ok := b.pending[key]
		if ok {
			delete(b.pending, key)
		}
		b.mu.Unlock()
		if !ok {
			b.log.Debug("Dropped a response to no known request", "id", string(m["id"]))
			return
		}
		m["id"] = p.id
		b.finish(ctx, p, m)
	}
}

// finish passes a response to the editor, binding new sessions first.
func (b *Bridge) finish(ctx context.Context, p pendingReq, m object) {
	_, failed := m["error"]
	result, hasResult := m["result"]
	ok := !failed && hasResult && string(result) != jsonNull
	switch p.method {
	case methodInitialize:
		if ok {
			b.mu.Lock()
			b.init = p.params
			if len(b.init) == 0 || string(b.init) == jsonNull {
				b.init = json.RawMessage("{}")
			}
			b.mu.Unlock()
			m["result"] = advertiseClose(result)
		}
	case methodNew, methodFork:
		if !ok {
			break
		}
		var res struct {
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(result, &res)
		if err := b.bind(ctx, res.SessionID, p.cwd); err != nil {
			b.writeEditor(bindError(p.id, p.cwd, err))
			if errors.Is(err, hubapi.ErrUnauthorized) {
				b.stop(err)
			}
			return
		}
		b.addSession(res.SessionID, p.cwd)
	case methodLoad, methodResume:
		if ok {
			b.addSession(p.session, p.cwd)
		}
	}
	b.writeEditor(mustJSON(m))
}

// advertiseClose adds sessionCapabilities.close to the agentCapabilities of
// an initialize result, since the bridge answers session/close itself. A
// result whose members are not objects is left as it is.
func advertiseClose(result json.RawMessage) json.RawMessage {
	res, ok := member(result)
	if !ok {
		return result
	}
	caps, ok := member(res["agentCapabilities"])
	if !ok {
		return result
	}
	sc, ok := member(caps["sessionCapabilities"])
	if !ok {
		return result
	}
	if c, has := sc["close"]; has && string(c) != jsonNull {
		return result
	}
	sc["close"] = json.RawMessage("{}")
	caps["sessionCapabilities"] = mustJSON(sc)
	res["agentCapabilities"] = mustJSON(caps)
	return mustJSON(res)
}

// member decodes a JSON object. A missing member or null is an empty
// object; any other value is not.
func member(raw json.RawMessage) (object, bool) {
	if len(raw) == 0 || string(raw) == jsonNull {
		return object{}, true
	}
	var o object
	if json.Unmarshal(raw, &o) != nil {
		return nil, false
	}
	return o, true
}

func (b *Bridge) addSession(id, cwd string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.sessions {
		if b.sessions[i].id == id {
			b.sessions[i].cwd = cwd
			return
		}
	}
	b.sessions = append(b.sessions, openSession{id: id, cwd: cwd})
}

// dropSession forgets a session the editor opened. It reports whether the
// session was open.
func (b *Bridge) dropSession(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.sessions {
		if b.sessions[i].id == id {
			b.sessions = append(b.sessions[:i], b.sessions[i+1:]...)
			return true
		}
	}
	return false
}

// closeSession answers session/close itself, because the hub refuses it
// (docs/protocol.md 6.1). ACP asks the agent to cancel the session's work
// as session/cancel does, so an open session gets a session/cancel on the
// current connection. A lost connection has no work left: its requests
// were answered with MsgLost.
func (b *Bridge) closeSession(ctx context.Context, m object, withID bool) {
	session := str(params(m)["sessionId"])
	if session == "" {
		if withID {
			b.writeEditor(errorMsg(m["id"], CodeInvalidParams, "talaria: sessionId must be a non-empty string"))
		}
		return
	}
	if b.dropSession(session) {
		b.cancelOnAgent(ctx, session)
	}
	if withID {
		b.writeEditor(mustJSON(object{keyJSONRPC: json.RawMessage(`"2.0"`), "id": m["id"], "result": json.RawMessage("{}")}))
	}
}

func (b *Bridge) cancelOnAgent(ctx context.Context, session string) {
	b.mu.Lock()
	conn, ready := b.conn, b.ready
	b.mu.Unlock()
	if !ready {
		return
	}
	data := mustJSON(object{
		keyJSONRPC: json.RawMessage(`"2.0"`), "method": mustJSON(methodCancel),
		"params": mustJSON(map[string]string{"sessionId": session}),
	})
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		b.log.Debug("Could not cancel a closed session's work", "session", session, "error", err)
	}
}

func (b *Bridge) stop(err error) {
	select {
	case b.fatal <- err:
	default:
	}
}

func (b *Bridge) ping(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(b.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, protocol.PingTimeout)
		err := conn.Ping(pctx)
		cancel()
		if err != nil && ctx.Err() == nil {
			b.log.Warn("No pong from the hub; dropping the ACP leg", "error", err)
			_ = conn.Close(protocol.ClosePingTimeout, "ping timeout")
			return
		}
	}
}

// writeEditor writes one JSON-RPC message to the editor as one line.
func (b *Bridge) writeEditor(data []byte) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		b.log.Error("Dropped a message that is not JSON", "error", err)
		return
	}
	buf.WriteByte('\n')
	b.outMu.Lock()
	defer b.outMu.Unlock()
	if _, err := b.opts.Out.Write(buf.Bytes()); err != nil {
		b.log.Warn("Could not write to the editor", "error", err)
	}
}

func errorMsg(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = json.RawMessage(jsonNull)
	}
	return mustJSON(map[string]any{keyJSONRPC: "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func hasID(m object) bool {
	id, ok := m["id"]
	return ok && string(id) != jsonNull
}

func params(m object) object {
	var p object
	_ = json.Unmarshal(m["params"], &p)
	return p
}

func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// queue is an unbounded FIFO of messages from one reader to one consumer.
type queue struct {
	mu     sync.Mutex
	items  [][]byte
	closed bool
	signal chan struct{}
}

func newQueue() *queue { return &queue{signal: make(chan struct{}, 1)} }

func (q *queue) push(data []byte) {
	q.mu.Lock()
	q.items = append(q.items, data)
	q.mu.Unlock()
	q.wake()
}

func (q *queue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.wake()
}

func (q *queue) wake() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

// pop returns the next message, waiting for one. It returns false once the
// queue is closed and empty.
func (q *queue) pop() ([]byte, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			data := q.items[0]
			q.items[0] = nil
			q.items = q.items[1:]
			q.mu.Unlock()
			return data, true
		}
		if q.closed {
			q.mu.Unlock()
			return nil, false
		}
		q.mu.Unlock()
		<-q.signal
	}
}
