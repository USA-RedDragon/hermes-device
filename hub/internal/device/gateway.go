package device

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/coder/websocket"
)

// sessionCheckTTL is how long a gateway leg trusts a lookup that found a
// session not bound to another device.
const sessionCheckTTL = 10 * time.Second

// bindCacheTTL is how long the hub remembers a create it bound, for the
// TUI's retry of the same request_id.
const bindCacheTTL = time.Hour

const ownCallTimeout = 15 * time.Second

// guardTimeout bounds each plugin call the guard makes, some of them on the
// device hop's reader: below the pong deadline, since pongs are read only
// while the reader runs.
const guardTimeout = 10 * time.Second

var errHubLegLost = errors.New("gateway leg lost")

type bindEntry struct {
	sessionID, dir string
	at             time.Time
}

func (s *Server) cachedCreate(deviceID, requestID, sessionID, dir string) bool {
	if requestID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.binds[deviceID+"\x00"+requestID]
	return ok && e.sessionID == sessionID && e.dir == dir && s.opts.Now().Sub(e.at) < bindCacheTTL
}

func (s *Server) rememberCreate(deviceID, requestID, sessionID, dir string) {
	if requestID == "" {
		return
	}
	now := s.opts.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.binds) > 4096 {
		for k, e := range s.binds {
			if now.Sub(e.at) >= bindCacheTTL {
				delete(s.binds, k)
			}
		}
	}
	s.binds[deviceID+"\x00"+requestID] = bindEntry{sessionID: sessionID, dir: dir, at: now}
}

func (s *Server) handleGateway(w http.ResponseWriter, r *http.Request) {
	d := deviceFrom(r)
	b := s.backends[d.BackendID]
	if s.isClosed() {
		apiError(w, http.StatusServiceUnavailable, codeBackendUnavailable, "the hub is shutting down")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dialTimeout)
	defer cancel()
	up, err := b.dialer.DialRaw(ctx)
	if err != nil {
		s.log.Warn("dial gateway leg", "device", d.ID, "backend", d.BackendID, "error", err.Error())
		apiError(w, http.StatusBadGateway, codeBackendUnavailable, "the hub could not reach this device's Hermes")
		return
	}
	up.SetReadLimit(maxMessage)
	down, err := websocket.Accept(w, r, nil)
	if err != nil {
		_ = up.Close(websocket.StatusGoingAway, "device upgrade failed")
		return
	}
	down.SetReadLimit(maxMessage)
	l := s.newLeg(r.Context(), d, "gateway", down, up)
	g := &guard{s: s, b: b, dev: d, l: l, pending: map[string]*pending{}, own: map[string]chan frame{},
		sessions: newSessionCheck(s, b, d, l), lost: make(chan struct{})}
	s.log.Info("device leg open", "leg", "gateway", "device", d.ID, "backend", d.BackendID)
	var loops sync.WaitGroup
	loops.Go(func() { g.loop(down, g.fromDevice, true) })
	loops.Go(func() { g.loop(up, g.fromGateway, false) })
	go func() {
		<-l.done
		g.markLost()
	}()
	l.run() //nolint:contextcheck // the leg outlives the upgrade request
	loops.Wait()
	g.work.Wait()
}

type heldKind int

const (
	heldCreate heldKind = iota
	heldResume
	heldWorkspace
	heldBranch
	heldList
)

// held is a forwarded device request whose answer the hub acts on before
// the device sees it.
type held struct {
	kind      heldKind
	id        json.RawMessage
	dir       string
	requestID string
	sessionID string
	prevDir   string
	method    string
}

// guard enforces the gateway leg rules of the hub: the device's token is
// not the user's operator access to their backend.
type guard struct {
	s   *Server
	b   *backend
	dev hub.Device
	l   *leg

	mu       sync.Mutex
	pending  map[string]*pending
	own      map[string]chan frame
	nextOwn  uint64
	sessions *sessionCheck
	capsOnce sync.Once
	caps     []string
	gone     bool
	lost     chan struct{}
	work     sync.WaitGroup
}

func (g *guard) markLost() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.gone {
		g.gone = true
		close(g.lost)
		g.pending = map[string]*pending{}
	}
}

func (g *guard) isLost() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gone
}

func (g *guard) loop(src *websocket.Conn, handle func(websocket.MessageType, []byte), fromDevice bool) {
	for {
		typ, data, err := src.Read(g.l.ctx)
		if err != nil {
			g.l.ended(err, fromDevice)
			return
		}
		handle(typ, data)
	}
}

func (g *guard) toDevice(data []byte) {
	if err := g.l.down.Write(g.l.ctx, websocket.MessageText, data); err != nil {
		g.s.log.Debug("write to device failed", "device", g.dev.ID, "error", err.Error())
	}
}

func (g *guard) toGateway(data []byte) {
	if err := g.l.up.Write(g.l.ctx, websocket.MessageText, data); err != nil {
		g.s.log.Debug("write to gateway failed", "device", g.dev.ID, "error", err.Error())
	}
}

func (g *guard) forward(f *frame, params map[string]json.RawMessage) {
	out, err := f.encode(params)
	if err != nil {
		g.toDevice(cwdAnswer(f.ID, err.Error()))
		return
	}
	g.toGateway(out)
}

// send forwards a device request under an id of the hub's own, so the
// answer is matched whatever form the gateway gives the id back in, and two
// requests with the same id cannot take each other's answer. h, when set,
// holds the answer for the hub to act on.
func (g *guard) send(f *frame, params map[string]json.RawMessage, h *held) {
	g.mu.Lock()
	g.nextOwn++
	own := fmt.Sprintf("%sd%d", OwnIDPrefix, g.nextOwn)
	g.pending[own] = &pending{id: f.ID, h: h}
	g.mu.Unlock()
	f.members["id"] = mustRaw(own)
	g.forward(f, params)
}

func (g *guard) take(own string) *pending {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.pending[own]
	delete(g.pending, own)
	return p
}

// pending is a device request the gateway has not answered yet.
type pending struct {
	id json.RawMessage
	h  *held
}

// fromDevice applies the rules to one device message, in order.
func (g *guard) fromDevice(typ websocket.MessageType, data []byte) {
	if typ != websocket.MessageText {
		g.s.log.Warn("dropped a binary message from a device", "device", g.dev.ID)
		return
	}
	f, err := parseFrame(data)
	if err != nil {
		g.toDevice(answerError(nil, codeInvalid, "hermes-hub takes one JSON-RPC object per message", nil))
		return
	}
	if _, own := ownID(f.ID); own {
		if f.Method != "" {
			g.toDevice(answerError(f.ID, codeInvalid, "ids starting with "+OwnIDPrefix+" are the hub's", nil))
		}
		return
	}
	if _, ok := f.members["method"]; !ok {
		g.answerServer(&f)
		return
	}
	params, isObject := decodeParams(f.Params)
	if !isObject {
		if f.hasID() {
			g.toDevice(answerError(f.ID, codeInvalidParams, "hermes-hub takes only object params", nil))
		}
		return
	}
	if !f.hasID() {
		if denied(f.Method, params) || rewritten(f.Method) || f.Method == methodCompletePath || f.Method == methodImageRender ||
			otherProfile(params) ||
			ownRequestID(params) || !rewriteParams(f.Method, params) {
			g.s.log.Warn("dropped a device notification", "device", g.dev.ID, "method", f.Method)
			return
		}
		if sid, err := g.checkSessions(params); err != nil {
			g.s.log.Warn("dropped a device notification for another device's session", "device", g.dev.ID,
				"method", f.Method, "session_id", sid)
			return
		}
		g.forward(&f, params)
		return
	}
	switch {
	case !validID(f.ID):
		g.toDevice(answerError(nil, codeInvalid, "a request id must be a string or a number", nil))
		return
	case f.Method == methodCompletePath:
		g.toDevice(answerResult(f.ID, map[string]any{"items": []any{}}))
		return
	case denied(f.Method, params):
		g.toDevice(deniedAnswer(f.ID, f.Method, params))
		return
	case otherProfile(params):
		g.toDevice(answerError(f.ID, codeDenied, "hermes-device serves only the device's own Hermes profile",
			map[string]any{keyReason: reasonDenied}))
		return
	case ownRequestID(params):
		g.toDevice(answerError(f.ID, codeInvalid, "request_id, submission_id and input_id values starting with "+OwnIDPrefix+" are the hub's", nil))
		return
	case !rewriteParams(f.Method, params):
		g.toDevice(answerError(f.ID, codeInvalidParams, "size params must be numbers", nil))
		return
	}
	if sid, err := g.checkSessions(params); err != nil {
		g.refuseSession(f.ID, sid, err)
		return
	}
	switch {
	case f.Method == methodCreate:
		g.rewriteCreate(&f, params)
	case f.Method == methodResume:
		dropMember(params, "editor", "mcp_servers")
		g.send(&f, params, &held{kind: heldResume, id: f.ID})
	case f.Method == methodMutate && rawString(params["operation"]) == opWorkspace:
		g.rewriteWorkspace(&f, params)
	case f.Method == methodMutate && rawString(params["operation"]) == opBranch:
		dropMember(params, "payload", "mcp_servers")
		g.send(&f, params, &held{kind: heldBranch, id: f.ID})
	default:
		g.sendChecked(&f, params)
	}
}

// sendChecked forwards a request whose answer the hub filters, or that
// needs a capability of the gateway.
func (g *guard) sendChecked(f *frame, params map[string]json.RawMessage) {
	switch {
	case f.Method == methodImageRender:
		g.work.Go(func() {
			if !g.hasCapability(capImageRenderConfined) {
				g.toDevice(answerError(f.ID, codeDenied, methodImageRender+" is not available through hermes-device: "+whyImageRender,
					map[string]any{keyReason: reasonDenied}))
				return
			}
			g.send(f, params, nil)
		})
	case slices.Contains(listMethods, f.Method):
		g.send(f, params, &held{kind: heldList, id: f.ID, method: f.Method})
	default:
		g.send(f, params, nil)
	}
}

// serverRequestPrefix starts the id of every request the gateway sends to
// its viewers (gateway/session_server_requests.py is_response_frame).
const serverRequestPrefix = "srq-"

// answerServer forwards a device's answer to a gateway request: a frame
// with no method member, a string id starting with srq- and exactly one
// of result and error. It is rebuilt from those members only. Anything
// else is dropped.
func (g *guard) answerServer(f *frame) {
	id, isString := rawStringOK(f.ID)
	_, hasResult := f.members["result"]
	_, hasError := f.members["error"]
	if !isString || !strings.HasPrefix(id, serverRequestPrefix) || hasResult == hasError {
		g.s.log.Warn("dropped a device message that is neither a request nor an answer to a gateway request", "device", g.dev.ID)
		return
	}
	out := map[string]json.RawMessage{"jsonrpc": mustRaw(jsonRPCVersion), "id": f.ID}
	if hasResult {
		out["result"] = f.members["result"]
	} else {
		out["error"] = f.members["error"]
	}
	if data, err := json.Marshal(out); err == nil {
		g.toGateway(data)
	}
}

var (
	errOtherDevice = errors.New("bound to another device")
	errNotBound    = errors.New("not bound to this device")
	// errBadSessionParam is a session id member that is not a string; the
	// gateway refuses those too.
	errBadSessionParam = errors.New("session id is not a string")
)

func (g *guard) refuseSession(id json.RawMessage, sid string, err error) {
	switch {
	case errors.Is(err, errBadSessionParam):
		g.toDevice(answerError(id, codeInvalidParams, sid+" must be a string", nil))
		return
	case errors.Is(err, errOtherDevice):
		g.s.log.Warn("refused a device request for another device's session", "device", g.dev.ID, "session_id", sid)
		g.toDevice(otherDeviceAnswer(id, sid))
		return
	case errors.Is(err, errNotBound):
		g.s.log.Warn("refused a resume of a session not bound to the device", "device", g.dev.ID, "session_id", sid)
		g.toDevice(notBoundAnswer(id, sid))
		return
	}
	g.s.log.Warn("could not check a session's binding", "device", g.dev.ID, "session_id", sid, "error", err.Error())
	g.toDevice(bindAnswer(id, err))
}

// checkSessions refuses a request whose session_id is not bound to this
// device (through its lineage), or whose parent_session_id is bound to
// another device. The device splice binds a session before it resumes or
// uses it, and the hub binds every session the leg creates.
func (g *guard) checkSessions(params map[string]json.RawMessage) (string, error) {
	for _, k := range []string{keySessionID, keyParentSession} {
		raw, ok := params[k]
		if !ok || isNull(raw) {
			continue
		}
		sid, isString := rawStringOK(raw)
		if !isString {
			return k, errBadSessionParam
		}
		if sid == "" {
			continue
		}
		if err := g.checkSession(sid, k == keySessionID); err != nil {
			return sid, err
		}
	}
	return "", nil
}

func (g *guard) checkSession(sid string, mine bool) error {
	return g.sessions.check(sid, mine)
}

func (g *guard) placeholder() (string, error) {
	ctx, cancel := context.WithTimeout(g.l.ctx, guardTimeout)
	defer cancel()
	return g.s.placeholderDir(ctx, g.b)
}

func (g *guard) bindCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), guardTimeout)
}

func (g *guard) bind(ctx context.Context, sid, dir string) error {
	_, err := g.b.plugin.Bind(ctx, sid, g.dev.ID, dir)
	if err == nil {
		g.sessions.mark(sid)
	}
	return err
}

// rewriteCreate runs session.create in the placeholder with the device
// directory as the row cwd, and holds the answer until the bind is done.
// A device splice may have rewritten it already, which leaves the device
// directory in row_cwd and the placeholder in cwd.
func (g *guard) rewriteCreate(f *frame, params map[string]json.RawMessage) {
	if isTrue(params["safe_mode"]) || isTrue(params["ignore_user_config"]) {
		g.toDevice(bypassAnswer(f.ID))
		return
	}
	ph, err := g.placeholder()
	if err != nil {
		g.s.log.Warn("no placeholder for session.create", "device", g.dev.ID, "error", err.Error())
		g.toDevice(bindAnswer(f.ID, err))
		return
	}
	cwd, row := rawString(params[keyCWD]), rawString(params["row_cwd"])
	dir := cwd
	if cwd == ph {
		dir = row
	}
	if !absDir(dir) || dir == ph {
		g.toDevice(cwdAnswer(f.ID, "session.create needs an absolute device directory as its cwd: run hermes-device chat in it"))
		return
	}
	params[keyCWD] = mustRaw(ph)
	params["row_cwd"] = mustRaw(dir)
	dropMember(params, "editor", "mcp_servers")
	g.send(f, params, &held{kind: heldCreate, id: f.ID, dir: dir, requestID: rawString(params[keyRequestID])})
}

// rewriteWorkspace binds the session to the new device directory before
// the move, and moves the session's cwd to the placeholder.
func (g *guard) rewriteWorkspace(f *frame, params map[string]json.RawMessage) {
	sid := rawString(params[keySessionID])
	payload, ok := decodeParams(params["payload"])
	dir := rawString(payload[keyCWD])
	if !ok || !absDir(dir) {
		g.toDevice(cwdAnswer(f.ID, "a workspace move needs an absolute device directory"))
		return
	}
	ph, err := g.placeholder()
	if err != nil {
		g.toDevice(bindAnswer(f.ID, err))
		return
	}
	h := &held{kind: heldWorkspace, id: f.ID, sessionID: sid}
	if dir != ph {
		ctx, cancel := g.bindCtx()
		if prev, bound, err := g.b.plugin.Binding(ctx, sid); err == nil && bound && prev.SessionID == sid && prev.DeviceID == g.dev.ID {
			h.prevDir = prev.Dir
		}
		err := g.bind(ctx, sid, dir)
		cancel()
		if err != nil {
			g.s.log.Warn("workspace bind failed", "device", g.dev.ID, "session_id", sid, "error", err.Error())
			g.toDevice(bindAnswer(f.ID, err))
			return
		}
		payload[keyCWD] = mustRaw(ph)
		params["payload"] = mustRaw(payload)
	}
	g.send(f, params, h)
}

// fromGateway relays gateway messages, takes the answers to the hub's own
// requests and acts on held answers.
func (g *guard) fromGateway(typ websocket.MessageType, data []byte) {
	f, err := parseFrame(data)
	if err != nil || f.Method != "" || !f.hasID() {
		g.relay(typ, data)
		return
	}
	own, ok := ownID(f.ID)
	if !ok {
		g.relay(typ, data)
		return
	}
	g.mu.Lock()
	ch := g.own[own]
	delete(g.own, own)
	g.mu.Unlock()
	if ch != nil {
		ch <- f
		return
	}
	p := g.take(own)
	if p == nil {
		return
	}
	f.members["id"] = p.id
	f.ID = p.id
	if data, err = json.Marshal(f.members); err != nil {
		return
	}
	h := p.h
	if h == nil {
		g.relay(typ, data)
		return
	}
	switch h.kind {
	case heldCreate:
		g.createAnswered(h, &f, data)
	case heldResume:
		g.resumeAnswered(h, &f, data)
	case heldBranch:
		g.branchAnswered(&f, data)
	case heldList:
		if f.isError() {
			g.toDevice(data)
			return
		}
		g.work.Go(func() { g.listAnswered(h.method, &f) })
	case heldWorkspace:
		if f.isError() && h.prevDir != "" {
			g.work.Go(func() {
				ctx, cancel := g.bindCtx()
				defer cancel()
				if err := g.bind(ctx, h.sessionID, h.prevDir); err != nil {
					g.s.log.Warn("restoring the binding after a refused workspace move failed", "device", g.dev.ID,
						"session_id", h.sessionID, "error", err.Error())
				}
			})
		}
		g.toDevice(data)
	}
}

func (g *guard) relay(typ websocket.MessageType, data []byte) {
	if err := g.l.down.Write(g.l.ctx, typ, data); err != nil {
		g.s.log.Debug("write to device failed", "device", g.dev.ID, "error", err.Error())
	}
}

type createResult struct {
	SessionID      string          `json:"session_id"`
	SubscriptionID string          `json:"subscription_id"`
	Revision       json.RawMessage `json:"revision"`
	Generation     json.RawMessage `json:"execution_generation"`
}

func (g *guard) createAnswered(h *held, f *frame, data []byte) {
	if f.isError() {
		g.b.forgetPlaceholder()
		g.toDevice(data)
		return
	}
	var res createResult
	if json.Unmarshal(f.Result, &res) != nil || res.SessionID == "" {
		g.s.log.Warn("session.create answered without a session_id", "device", g.dev.ID)
		g.toDevice(data)
		return
	}
	g.work.Go(func() {
		if g.s.cachedCreate(g.dev.ID, h.requestID, res.SessionID, h.dir) {
			g.toDevice(data)
			return
		}
		ctx, cancel := g.bindCtx()
		defer cancel()
		err := g.s.bindAllowed(ctx, g.b, g.dev.ID, res.SessionID)
		if err == nil {
			err = g.bind(ctx, res.SessionID, h.dir)
		}
		if err == nil {
			g.s.noteCreated(g.dev.ID, res.SessionID)
			g.s.rememberCreate(g.dev.ID, h.requestID, res.SessionID, h.dir)
			g.toDevice(data)
			return
		}
		if g.isLost() {
			g.s.log.Warn("bind after session.create failed with the gateway leg gone", "device", g.dev.ID,
				"session_id", res.SessionID, "error", err.Error())
			return
		}
		if errors.Is(err, errOtherDevice) {
			g.s.log.Warn("session.create answered with another device's session", "device", g.dev.ID, "session_id", res.SessionID)
			g.detach(res.SessionID, res.SubscriptionID)
			g.toDevice(otherDeviceAnswer(h.id, res.SessionID))
			return
		}
		fresh, rerr := g.s.neverBound(ctx, g.b, res.SessionID)
		if rerr != nil || !fresh {
			g.s.log.Warn("bind after session.create failed; keeping the session, which was or is bound", "device", g.dev.ID,
				"session_id", res.SessionID, "error", err.Error())
			g.detach(res.SessionID, res.SubscriptionID)
			g.toDevice(bindAnswer(h.id, err))
			return
		}
		g.s.log.Warn("bind after session.create failed; deleting the session", "device", g.dev.ID,
			"session_id", res.SessionID, "error", err.Error())
		if g.deleteSession(res) {
			g.toDevice(bindAnswer(h.id, err))
		}
	})
}

// branchAnswered remembers the child of a branch as created by the device,
// so the device may bind it before its lineage row is written.
func (g *guard) branchAnswered(f *frame, data []byte) {
	var res struct {
		Child string `json:"branched_session_id"`
	}
	if !f.isError() && json.Unmarshal(f.Result, &res) == nil && res.Child != "" {
		g.s.noteCreated(g.dev.ID, res.Child)
	}
	g.toDevice(data)
}

func (g *guard) resumeAnswered(h *held, f *frame, data []byte) {
	var res struct {
		SessionID      string `json:"session_id"`
		SubscriptionID string `json:"subscription_id"`
	}
	if f.isError() || json.Unmarshal(f.Result, &res) != nil || res.SessionID == "" {
		g.toDevice(data)
		return
	}
	g.work.Go(func() {
		err := g.checkSession(res.SessionID, true)
		if err == nil {
			g.toDevice(data)
			return
		}
		g.detach(res.SessionID, res.SubscriptionID)
		g.refuseSession(h.id, res.SessionID, err)
	})
}

// detach ends the hub leg's subscription to a session the device may not
// see, so the gateway sends no more of its events on the leg.
func (g *guard) detach(sid, subscription string) {
	if subscription != "" {
		_, _ = g.call(methodDetach, map[string]any{keySessionID: sid, "subscription_id": subscription})
	}
}

// deleteSession deletes a session whose bind failed and waits for the
// answer. It returns false when the leg went away first.
func (g *guard) deleteSession(res createResult) bool {
	rev, gen := res.Revision, res.Generation
	if len(rev) == 0 {
		rev = json.RawMessage("null")
	}
	if len(gen) == 0 {
		gen = json.RawMessage("null")
	}
	ans, err := g.call(methodMutate, map[string]any{
		keySessionID: res.SessionID, "request_id": OwnIDPrefix + "delete-" + res.SessionID,
		"expected_revision": rev, "expected_generation": gen, "operation": opDelete, "payload": map[string]any{},
	})
	switch {
	case err != nil:
		g.s.log.Warn("deleting the unbound session failed", "device", g.dev.ID, "session_id", res.SessionID, "error", err.Error())
		return !g.isLost()
	case ans.isError():
		g.s.log.Warn("gateway refused to delete the unbound session", "device", g.dev.ID, "session_id", res.SessionID,
			"error", string(ans.Error))
	}
	return true
}

// call sends a request of the hub's own and waits for its answer.
func (g *guard) call(method string, params any) (frame, error) {
	ch := make(chan frame, 1)
	g.mu.Lock()
	if g.gone {
		g.mu.Unlock()
		return frame{}, fmt.Errorf("%s: %w", method, errHubLegLost)
	}
	g.nextOwn++
	id := fmt.Sprintf("%s%d", OwnIDPrefix, g.nextOwn)
	g.own[id] = ch
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.own, id)
		g.mu.Unlock()
	}()
	msg, err := json.Marshal(map[string]any{"jsonrpc": jsonRPCVersion, "id": id, "method": method, "params": params})
	if err != nil {
		return frame{}, err
	}
	ctx, cancel := context.WithTimeout(g.l.ctx, ownCallTimeout)
	defer cancel()
	if err := g.l.up.Write(ctx, websocket.MessageText, msg); err != nil {
		return frame{}, fmt.Errorf("%s: %w", method, err)
	}
	select {
	case f := <-ch:
		return f, nil
	case <-g.lost:
		return frame{}, fmt.Errorf("%s: %w", method, errHubLegLost)
	case <-ctx.Done():
		return frame{}, fmt.Errorf("%s: %w", method, ctx.Err())
	}
}

// listMethods are the methods whose answers list sessions of the whole
// profile. The hub keeps only the rows of this device's sessions.
//
//nolint:gochecknoglobals // a fixed list
var listMethods = []string{methodList, methodActiveList, methodMostRecent}

// listAnswered keeps only the sessions bound to this device (through their
// lineage) in a list answer. A session whose binding cannot be read is
// left out.
func (g *guard) listAnswered(method string, f *frame) {
	res, isObj := decodeParams(f.Result)
	if !isObj {
		res = map[string]json.RawMessage{}
	}
	mine := func(sid string) bool { return sid != "" && g.sessions.check(sid, true) == nil }
	if method == methodMostRecent {
		if sid, _ := rawStringOK(res[keySessionID]); !mine(sid) {
			res = map[string]json.RawMessage{keySessionID: json.RawMessage("null")}
		}
	} else {
		var rows []json.RawMessage
		_ = json.Unmarshal(res["sessions"], &rows)
		kept := []json.RawMessage{}
		for _, row := range rows {
			r, _ := decodeParams(row)
			sid, ok := rawStringOK(r[keySessionID])
			if !ok {
				sid, _ = rawStringOK(r["id"])
			}
			if mine(sid) {
				kept = append(kept, row)
			}
		}
		res["sessions"] = mustRaw(kept)
	}
	f.members["result"] = mustRaw(res)
	if out, err := json.Marshal(f.members); err == nil {
		g.toDevice(out)
	}
}

// hasCapability reports whether the gateway lists capability in
// runtime.describe. The answer is read once per leg; when it cannot be
// read, the gateway has no capabilities.
func (g *guard) hasCapability(capability string) bool {
	g.capsOnce.Do(func() {
		f, err := g.call("runtime.describe", map[string]any{})
		if err != nil || f.isError() {
			return
		}
		var res struct {
			Capabilities []string `json:"capabilities"`
		}
		if json.Unmarshal(f.Result, &res) == nil {
			g.caps = res.Capabilities
		}
	})
	return slices.Contains(g.caps, capability)
}
