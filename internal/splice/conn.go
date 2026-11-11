package splice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

type heldKind int

const (
	heldCreate heldKind = iota
	heldWorkspace
	heldBranch
)

// held is a forwarded TUI request whose answer the splice acts on before
// the TUI sees it.
type held struct {
	kind heldKind
	id   json.RawMessage
	// dir is the device directory of a create.
	dir       string
	requestID string
	// sessionID is the session a workspace move or branch names.
	sessionID string
	// prevDir is the session's device directory before a workspace move.
	prevDir string
}

// conn is one TUI socket and its gateway leg.
type conn struct {
	s     *Server
	token *tokenEntry
	tui   *websocket.Conn
	hub   *websocket.Conn

	ctx    context.Context //nolint:containedctx // per-connection lifetime shared by the copyLoop handlers
	cancel context.CancelFunc

	mu      sync.Mutex
	pending map[string]*held
	own     map[string]chan frame
	nextOwn uint64
	hubDown bool
	hubGone chan struct{}

	// work counts goroutines that finish held answers.
	work      sync.WaitGroup
	closeOnce sync.Once
}

func newConn(s *Server, token *tokenEntry, tui, hub *websocket.Conn) *conn {
	return &conn{
		s: s, token: token, tui: tui, hub: hub,
		pending: map[string]*held{}, own: map[string]chan frame{}, hubGone: make(chan struct{}),
	}
}

func (c *conn) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	c.ctx, c.cancel = ctx, cancel
	defer c.cancel()
	tuiDone := make(chan error, 1)
	hubDone := make(chan error, 1)
	go func() { tuiDone <- c.copyLoop(c.tui, c.fromTUI) }()
	go func() { hubDone <- c.copyLoop(c.hub, c.fromHub) }()
	go c.pinger(ctx, c.tui)
	go c.pinger(ctx, c.hub)

	tuiEnded := false
	select {
	case err := <-tuiDone:
		tuiEnded = true
		c.s.log.Debug("TUI socket ended", "error", err)
		c.cancel()
		_ = c.hub.Close(websocket.StatusNormalClosure, "")
		if err := <-hubDone; err != nil {
			c.s.log.Debug("gateway leg closed", "error", err)
		}
	case err := <-hubDone:
		c.s.log.Info("gateway leg ended", "error", err)
	}
	c.hubLost()
	c.work.Wait()
	c.closeTUI(websocket.StatusInternalError, "hub gateway leg lost")
	if !tuiEnded {
		if err := <-tuiDone; err != nil {
			c.s.log.Debug("TUI socket closed", "error", err)
		}
	}
	_ = c.hub.CloseNow()
	_ = c.tui.CloseNow()
}

func (c *conn) copyLoop(ws *websocket.Conn, handle func(websocket.MessageType, []byte)) error {
	for {
		typ, data, err := ws.Read(c.ctx)
		if err != nil {
			return err
		}
		handle(typ, data)
	}
}

// closeTUI starts the close handshake with the TUI without waiting for it.
func (c *conn) closeTUI(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() { go func() { _ = c.tui.Close(code, reason) }() })
}

func (c *conn) pinger(ctx context.Context, ws *websocket.Conn) {
	t := time.NewTicker(c.s.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, c.s.opts.PingTimeout)
		err := ws.Ping(pingCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				c.s.log.Info("no pong; closing", "error", err)
				go func() { _ = ws.Close(protocol.ClosePingTimeout, "ping timeout") }()
				time.AfterFunc(c.s.opts.PingTimeout, func() { _ = ws.CloseNow() })
			}
			return
		}
	}
}

func (c *conn) toTUI(data []byte) {
	if err := c.tui.Write(c.ctx, websocket.MessageText, data); err != nil {
		c.s.log.Debug("write to TUI failed", "error", err)
	}
}

func (c *conn) forward(typ websocket.MessageType, data []byte) {
	if err := c.hub.Write(c.ctx, typ, data); err != nil {
		c.s.log.Debug("write to hub failed", "error", err)
	}
}

func (c *conn) hold(key string, h *held) {
	c.mu.Lock()
	c.pending[key] = h
	c.mu.Unlock()
}

func (c *conn) take(key string) *held {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.pending[key]
	delete(c.pending, key)
	return h
}

func (c *conn) isHubDown() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hubDown
}

// hubLost runs once the gateway leg ended. Held requests get no answer:
// the TUI sees its socket close (1011), and it resends a create with the
// same request_id, while a JSON-RPC error would make it mint a new one.
func (c *conn) hubLost() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hubDown {
		return
	}
	c.hubDown = true
	close(c.hubGone)
	c.pending = map[string]*held{}
}

// bindCtx outlives the TUI socket, so a bind in flight still completes and
// fills the bind cache for a retry.
func (c *conn) bindCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(c.ctx), dialTimeout)
}

// rewritten names the methods whose params the splice changes or reads.
func rewritten(method string) bool {
	return method == methodCreate || method == methodResume || method == methodMutate
}

func (c *conn) fromTUI(typ websocket.MessageType, data []byte) {
	f, err := parseFrame(data)
	if err != nil {
		c.s.log.Warn("dropped a TUI message that is not a JSON object", "error", err)
		return
	}
	if f.Method == "" {
		c.forward(typ, data)
		return
	}
	params, objectParams := decodeParams(f.Params)
	if !f.hasID() {
		if denied(f.Method, params) || rewritten(f.Method) || f.Method == methodCompletePath {
			c.s.log.Warn("dropped a TUI notification", "method", f.Method)
			return
		}
		c.forward(typ, data)
		return
	}
	key, ok := idKey(f.ID)
	switch {
	case !ok:
		if rewritten(f.Method) || denied(f.Method, params) {
			c.s.log.Warn("dropped a TUI request with an id that is not a string or number", "method", f.Method)
			return
		}
		c.forward(typ, data)
	case f.Method == methodCompletePath:
		c.toTUI(answerResult(f.ID, map[string]any{"items": []any{}}))
	case denied(f.Method, params):
		c.toTUI(deniedAnswer(f.ID, f.Method))
	case rewritten(f.Method) && !objectParams:
		c.toTUI(cwdAnswer(f.ID, f.Method+" params must be an object"))
	case f.Method == methodCreate:
		c.rewriteCreate(f.ID, key, params, data)
	case f.Method == methodResume:
		c.rewriteResume(f.ID, params, data)
	case f.Method == methodMutate && rawString(params["operation"]) == opWorkspace:
		c.rewriteWorkspace(f.ID, key, params, data)
	case f.Method == methodMutate && rawString(params["operation"]) == opBranch:
		dropMember(params, "payload", "mcp_servers")
		c.hold(key, &held{kind: heldBranch, id: f.ID, sessionID: rawString(params[keySessionID])})
		c.forwardParams(f.ID, data, params)
	default:
		c.forward(typ, data)
	}
}

// forwardParams forwards data with its params replaced.
func (c *conn) forwardParams(id json.RawMessage, data []byte, params map[string]json.RawMessage) {
	out, err := withParams(data, params)
	if err != nil {
		c.toTUI(cwdAnswer(id, err.Error()))
		return
	}
	c.forward(websocket.MessageText, out)
}

func (c *conn) rewriteCreate(id json.RawMessage, key string, params map[string]json.RawMessage, data []byte) {
	dir := rawString(params["cwd"])
	if dir == "" || !filepath.IsAbs(dir) {
		c.toTUI(cwdAnswer(id, "session.create needs an absolute cwd: run talaria chat in the device directory"))
		return
	}
	if isTrue(params["safe_mode"]) || isTrue(params["ignore_user_config"]) {
		c.toTUI(bypassAnswer(id))
		return
	}
	ph, err := c.s.placeholderDir(c.ctx)
	if err != nil {
		c.s.log.Warn("whoami failed; no placeholder for session.create", "error", err)
		c.toTUI(bindAnswer(id, err))
		return
	}
	params["row_cwd"] = mustRaw(dir)
	params["cwd"] = mustRaw(ph)
	dropMember(params, "editor", "mcp_servers")
	c.hold(key, &held{kind: heldCreate, id: id, dir: dir, requestID: rawString(params["request_id"])})
	c.forwardParams(id, data, params)
}

// rewriteResume resumes by an explicit session id, which the hub needs:
// it looks up a resume by title, of the latest session or with an empty
// session_id, and the session a session_id names (which may be a title),
// then binds the session to the directory chat runs in, unless this chat
// already bound it (a resume after a reconnect).
func (c *conn) rewriteResume(id json.RawMessage, params map[string]json.RawMessage, data []byte) {
	dropMember(params, "editor", "mcp_servers")
	sid := rawString(params[keySessionID])
	_, latest := params["latest"]
	_, byTitle := params["title"]
	if !latest && !byTitle && sid != "" && c.token.saw(sid) {
		c.forwardParams(id, data, params)
		return
	}
	if c.isHubDown() {
		return
	}
	// A lookup can take many calls, so it runs off the TUI reader, which
	// must keep reading for the TUI's pongs.
	c.work.Go(func() { c.resolveResume(id, params, data, sid, latest, byTitle) })
}

// resolveResume looks up the session of a resume, binds it and forwards
// the resume by its id.
func (c *conn) resolveResume(id json.RawMessage, params map[string]json.RawMessage, data []byte, sid string,
	latest, byTitle bool,
) {
	var err error
	switch {
	case latest || (!byTitle && sid == ""):
		sid, err = resolveLatest(c)
	case byTitle:
		sid, err = resolveName(c, rawString(params["title"]))
	default:
		var found string
		if found, err = resolveName(c, sid); err == nil {
			sid = found
		} else if errors.Is(err, ErrNoSession) {
			// Not a title or a local session id: resume the id as sent.
			err = nil
		}
	}
	switch {
	case errors.Is(err, ErrNoSession) && (latest || !byTitle):
		c.toTUI(notFoundAnswer(id, "no session bound to this device to resume"))
		return
	case errors.Is(err, ErrNoSession):
		c.toTUI(notFoundAnswer(id, "no session matches "+strconv.Quote(rawString(params["title"]))))
		return
	case err != nil:
		if !c.isHubDown() {
			c.toTUI(lookupAnswer(id, err))
		}
		return
	}
	delete(params, "latest")
	delete(params, "title")
	delete(params, "workspace")
	params[keySessionID] = mustRaw(sid)
	if !c.token.saw(sid) {
		if err := c.s.bind(c.ctx, sid, c.token.dir); err != nil {
			c.s.log.Warn("bind before session.resume failed", keySessionID, sid, "error", err)
			c.toTUI(bindAnswer(id, err))
			return
		}
		c.token.see(sid)
	}
	c.forwardParams(id, data, params)
}

func (c *conn) rewriteWorkspace(id json.RawMessage, key string, params map[string]json.RawMessage, data []byte) {
	sid := rawString(params[keySessionID])
	payload, _ := decodeParams(params["payload"])
	dir := rawString(payload["cwd"])
	if dir == "" || !filepath.IsAbs(dir) {
		c.toTUI(cwdAnswer(id, "a workspace move needs an absolute device directory"))
		return
	}
	if c.isHubDown() {
		return
	}
	ph, err := c.s.placeholderDir(c.ctx)
	if err != nil {
		c.toTUI(bindAnswer(id, err))
		return
	}
	prev, _ := c.s.sessionDir(sid)
	if err := c.s.bind(c.ctx, sid, dir); err != nil {
		c.s.log.Warn("workspace bind failed", keySessionID, sid, "error", err)
		c.toTUI(bindAnswer(id, err))
		return
	}
	c.token.see(sid)
	payload["cwd"] = mustRaw(ph)
	params["payload"] = mustRaw(payload)
	c.hold(key, &held{kind: heldWorkspace, id: id, sessionID: sid, prevDir: prev})
	c.forwardParams(id, data, params)
}

func isError(f *frame) bool {
	return len(f.Error) > 0 && !isNull(f.Error)
}

func (c *conn) fromHub(typ websocket.MessageType, data []byte) {
	f, err := parseFrame(data)
	if err != nil || f.Method != "" || !f.hasID() {
		c.relay(typ, data)
		return
	}
	if own, ok := ownID(f.ID); ok {
		c.mu.Lock()
		ch := c.own[own]
		delete(c.own, own)
		c.mu.Unlock()
		if ch != nil {
			ch <- f
		}
		return
	}
	key, ok := idKey(f.ID)
	if !ok {
		c.relay(typ, data)
		return
	}
	h := c.take(key)
	if h == nil {
		c.relay(typ, data)
		return
	}
	switch h.kind {
	case heldCreate:
		c.createAnswered(h, &f, data)
	case heldWorkspace:
		if isError(&f) && h.prevDir != "" {
			c.work.Go(func() {
				ctx, cancel := c.bindCtx()
				defer cancel()
				if err := c.s.bind(ctx, h.sessionID, h.prevDir); err != nil {
					c.s.log.Warn("restoring the binding after a refused workspace move failed",
						keySessionID, h.sessionID, "error", err)
				}
			})
		}
		c.toTUI(data)
	case heldBranch:
		c.branchAnswered(h, &f, data)
	}
}

func (c *conn) relay(typ websocket.MessageType, data []byte) {
	if err := c.tui.Write(c.ctx, typ, data); err != nil {
		c.s.log.Debug("write to TUI failed", "error", err)
	}
}

type createResult struct {
	SessionID  string          `json:"session_id"`
	Revision   json.RawMessage `json:"revision"`
	Generation json.RawMessage `json:"execution_generation"`
}

func (c *conn) createAnswered(h *held, f *frame, data []byte) {
	if isError(f) {
		c.s.forgetPlaceholder()
		c.toTUI(data)
		return
	}
	var res createResult
	if json.Unmarshal(f.Result, &res) != nil || res.SessionID == "" {
		c.s.log.Warn("session.create answered without a session_id")
		c.toTUI(data)
		return
	}
	c.work.Go(func() {
		if c.s.cachedCreate(h.requestID, res.SessionID, h.dir) {
			c.token.see(res.SessionID)
			c.toTUI(data)
			return
		}
		ctx, cancel := c.bindCtx()
		defer cancel()
		err := c.s.bind(ctx, res.SessionID, h.dir)
		if err == nil {
			c.s.rememberCreate(h.requestID, res.SessionID, h.dir)
			c.token.see(res.SessionID)
			c.toTUI(data)
			return
		}
		if c.isHubDown() {
			// No delete is possible and an error would be final; the TUI
			// retries the same request_id on a new socket.
			c.s.log.Warn("bind after session.create failed with the gateway leg gone", keySessionID, res.SessionID, "error", err)
			return
		}
		c.s.log.Warn("bind after session.create failed; deleting the session", keySessionID, res.SessionID, "error", err)
		if c.deleteSession(res) {
			c.toTUI(bindAnswer(h.id, err))
		}
	})
}

func (c *conn) branchAnswered(h *held, f *frame, data []byte) {
	var res struct {
		Branched string `json:"branched_session_id"`
	}
	if isError(f) || json.Unmarshal(f.Result, &res) != nil || res.Branched == "" {
		c.toTUI(data)
		return
	}
	dir, ok := c.s.sessionDir(h.sessionID)
	if !ok {
		c.s.log.Info("branch of a session this daemon did not bind; the child inherits through lineage",
			keySessionID, h.sessionID, "branched_session_id", res.Branched)
		c.toTUI(data)
		return
	}
	c.work.Go(func() {
		ctx, cancel := c.bindCtx()
		defer cancel()
		if err := c.s.bind(ctx, res.Branched, dir); err != nil {
			c.s.log.Warn("binding a branched session failed; lineage still fails it closed",
				"branched_session_id", res.Branched, "error", err)
		} else {
			c.token.see(res.Branched)
		}
		c.toTUI(data)
	})
}

// deleteSession sends the session.mutate delete of docs/protocol.md 5.3
// and waits for its answer. A refusal is logged, not retried. It returns
// false when the gateway leg went away first, so the create stays
// unanswered and the TUI retries it.
func (c *conn) deleteSession(res createResult) bool {
	rev, gen := res.Revision, res.Generation
	if len(rev) == 0 {
		rev = json.RawMessage("null")
	}
	if len(gen) == 0 {
		gen = json.RawMessage("null")
	}
	params := map[string]json.RawMessage{
		keySessionID:          mustRaw(res.SessionID),
		"request_id":          mustRaw(OwnIDPrefix + "delete-" + res.SessionID),
		"expected_revision":   rev,
		"expected_generation": gen,
		"operation":           mustRaw(opDelete),
		"payload":             json.RawMessage("{}"),
	}
	ans, err := c.call(methodMutate, params, deleteTimeout)
	switch {
	case err != nil:
		c.s.log.Warn("deleting the unbound session failed", keySessionID, res.SessionID, "error", err)
		return !c.isHubDown()
	case isError(&ans):
		c.s.log.Warn("gateway refused to delete the unbound session", keySessionID, res.SessionID,
			"error", string(ans.Error))
	}
	return true
}

// call sends a request of the splice's own to the gateway and waits for
// its answer.
func (c *conn) call(method string, params map[string]json.RawMessage, timeout time.Duration) (frame, error) {
	ch := make(chan frame, 1)
	c.mu.Lock()
	if c.hubDown {
		c.mu.Unlock()
		return frame{}, fmt.Errorf("%s: gateway leg lost", method)
	}
	c.nextOwn++
	id := fmt.Sprintf("%s%d", OwnIDPrefix, c.nextOwn)
	c.own[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.own, id)
		c.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), timeout)
	defer cancel()
	return sendCall(ctx, c.hub, id, method, params, ch, c.hubGone)
}
