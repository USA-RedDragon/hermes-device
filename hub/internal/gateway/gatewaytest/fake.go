// Package gatewaytest is an in-process fake of the Hermes v0.22.7 gateway
// Webapp listener: password login, WebSocket tickets and the subset of the
// canonical session contract the hub uses. Its checks on params follow
// gateway/session_controls.py.
package gatewaytest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/coder/websocket"
)

const (
	kSessionID       = "session_id"
	kJSONRPC         = "jsonrpc"
	kEvent           = "event"
	kMethod          = "method"
	kToolID          = "tool_id"
	kText            = "text"
	kStatus          = "status"
	kName            = "name"
	kResult          = "result"
	kReplayEpoch     = "replay_epoch"
	kLastSequence    = "last_sequence"
	kExecGen         = "execution_generation"
	errInvalidParams = "invalid_params"
	jsonRPCVersion   = "2.0"
	toolReadFile     = "read_file"
)

// Call is one request the fake received.
type Call struct {
	Method string
	Params map[string]any
}

// Session is a fake gateway session.
type Session struct {
	ID            string
	Title         string
	Live          bool
	Running       bool
	StartedAt     float64
	Messages      []map[string]any
	Generation    int64
	ShowReasoning bool

	epoch   string
	seq     int64
	ring    []gateway.Event
	prompts map[string]gateway.Prompt
	subs    map[*conn]string
}

// Gateway is the fake. Lock-free methods are safe for concurrent use.
type Gateway struct {
	Server   *httptest.Server
	Username string
	Password string

	mu       sync.Mutex
	sessions map[string]*Session
	tickets  map[string]bool
	cookies  map[string]bool
	conns    map[*conn]bool
	calls    []Call
	logins   int
	// HoldTurns stops prompt.submit from running its scripted turn.
	HoldTurns bool
}

type conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
}

//nolint:contextcheck // a test fake: every write gets its own deadline
func (c *conn) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.ws.Write(ctx, websocket.MessageText, b)
}

func randHex() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// New starts a fake gateway with the given dashboard credentials.
func New(username, password string) *Gateway {
	g := &Gateway{
		Username: username, Password: password,
		sessions: map[string]*Session{}, tickets: map[string]bool{},
		cookies: map[string]bool{}, conns: map[*conn]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/password-login", g.login)
	mux.HandleFunc("POST /api/auth/ws-ticket", g.ticket)
	mux.HandleFunc("GET /api/ws", g.ws)
	g.Server = httptest.NewServer(mux)
	return g
}

// URL is the fake's base URL.
func (g *Gateway) URL() string { return g.Server.URL }

// Close stops the fake.
func (g *Gateway) Close() {
	g.DropConnections()
	g.Server.Close()
}

// AddSession adds a session.
func (g *Gateway) AddSession(s *Session) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s.epoch = randHex()
	s.prompts = map[string]gateway.Prompt{}
	s.subs = map[*conn]string{}
	g.sessions[s.ID] = s
}

// Calls returns every request received so far.
func (g *Gateway) Calls() []Call {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.calls)
}

// Logins is how many password logins succeeded.
func (g *Gateway) Logins() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.logins
}

// ShowReasoning reports a session's show_reasoning flag.
func (g *Gateway) ShowReasoning(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sessions[id].ShowReasoning
}

// DropConnections closes every open WebSocket without a close handshake.
func (g *Gateway) DropConnections() {
	g.mu.Lock()
	conns := make([]*conn, 0, len(g.conns))
	for c := range g.conns {
		conns = append(conns, c)
	}
	g.mu.Unlock()
	for _, c := range conns {
		_ = c.ws.CloseNow()
	}
}

// ConnCount is the number of open WebSockets.
func (g *Gateway) ConnCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.conns)
}

// Publish emits one session event to subscribers and the replay ring.
func (g *Gateway) Publish(sessionID, typ string, payload any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.publishLocked(g.sessions[sessionID], typ, payload)
}

// RetireSubscriptions drops every subscription to a session and sends
// session.replay_gap, as the gateway does when a viewer's backlog overflows.
func (g *Gateway) RetireSubscriptions(sessionID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[sessionID]
	for c := range s.subs {
		c.send(map[string]any{kJSONRPC: jsonRPCVersion, kMethod: kEvent, "params": map[string]any{
			"type": gateway.ReplayGapEvent, kSessionID: s.ID,
			"payload": map[string]any{kReplayEpoch: s.epoch, "latest_seq": s.seq}}})
	}
	s.subs = map[*conn]string{}
}

// PublishOffline adds an event to the replay ring without sending it to
// any connection, as if it was published while the hub was away.
func (g *Gateway) PublishOffline(sessionID, typ string, payload any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[sessionID]
	subs := s.subs
	s.subs = map[*conn]string{}
	g.publishLocked(s, typ, payload)
	s.subs = subs
}

func (g *Gateway) publishLocked(s *Session, typ string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	s.seq++
	gen := s.Generation
	ev := gateway.Event{Type: typ, SessionID: s.ID, Payload: raw, ReplayEpoch: s.epoch, Seq: s.seq, ExecutionGeneration: &gen}
	s.ring = append(s.ring, ev)
	for c := range s.subs {
		c.send(map[string]any{kJSONRPC: jsonRPCVersion, kMethod: kEvent, "params": ev})
	}
}

func (g *Gateway) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider, Username, Password string
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.Provider != "basic" ||
		body.Username != g.Username || body.Password != g.Password {
		http.Error(w, `{"detail":"Invalid credentials"}`, http.StatusUnauthorized)
		return
	}
	v := randHex()
	g.mu.Lock()
	g.cookies[v] = true
	g.logins++
	g.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "hermes_session_at", Value: v, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}) //nolint:gosec // plain-HTTP test server
	_, _ = w.Write([]byte(`{"ok":true,"next":"/"}`))
}

func (g *Gateway) ticket(w http.ResponseWriter, r *http.Request) {
	ck, err := r.Cookie("hermes_session_at")
	g.mu.Lock()
	ok := err == nil && g.cookies[ck.Value]
	t := randHex()
	if ok {
		g.tickets[t] = true
	}
	g.mu.Unlock()
	if !ok {
		http.Error(w, `{"detail":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"ticket": t, "ttl_seconds": 30}); err != nil {
		panic(err)
	}
}

// ExpireLogins forgets every dashboard session cookie.
func (g *Gateway) ExpireLogins() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cookies = map[string]bool{}
}

func (g *Gateway) ws(w http.ResponseWriter, r *http.Request) {
	var ticket string
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(p), gateway.TicketPrefix); ok {
			ticket = t
		}
	}
	g.mu.Lock()
	ok := g.tickets[ticket]
	delete(g.tickets, ticket)
	g.mu.Unlock()
	if !ok {
		http.Error(w, "ticket", http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{gateway.Subprotocol}})
	if err != nil {
		return
	}
	ws.SetReadLimit(64 << 20)
	c := &conn{ws: ws}
	g.mu.Lock()
	g.conns[c] = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(g.conns, c)
		for _, s := range g.sessions {
			delete(s.subs, c)
		}
		g.mu.Unlock()
		_ = ws.CloseNow()
	}()
	c.send(map[string]any{kJSONRPC: jsonRPCVersion, kMethod: kEvent, "params": map[string]any{
		"type": gateway.ReadyEvent, "payload": map[string]any{"session_authority": true, kReplayEpoch: "x"}}})
	for {
		_, data, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(data, &req) != nil {
			continue
		}
		g.mu.Lock()
		g.calls = append(g.calls, Call{Method: req.Method, Params: req.Params})
		result, rerr, after := g.dispatch(c, req.Method, req.Params)
		g.mu.Unlock()
		if rerr != "" {
			c.send(map[string]any{kJSONRPC: jsonRPCVersion, "id": req.ID, "error": map[string]any{"code": -32000, "message": rerr}})
		} else {
			c.send(map[string]any{kJSONRPC: jsonRPCVersion, "id": req.ID, kResult: result})
		}
		if after != nil {
			go after()
		}
	}
}

func keys(p map[string]any) []string {
	out := make([]string, 0, len(p))
	for k := range p {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func onlyKeys(p map[string]any, allowed ...string) bool {
	for k := range p {
		if !slices.Contains(allowed, k) {
			return false
		}
	}
	return true
}

func str(p map[string]any, k string) string {
	s, _ := p[k].(string)
	return s
}

func num(p map[string]any, k string) (int64, bool) {
	f, ok := p[k].(float64)
	return int64(f), ok
}

func (g *Gateway) snapshotLocked(c *conn, s *Session) map[string]any {
	sub := randHex()
	s.subs[c] = sub
	prompts := make([]gateway.Prompt, 0, len(s.prompts))
	for _, p := range s.prompts {
		prompts = append(prompts, p)
	}
	msgs := s.Messages
	if msgs == nil {
		msgs = []map[string]any{}
	}
	return map[string]any{
		kSessionID: s.ID, "stored_session_id": s.ID, "messages": msgs, "message_count": len(msgs),
		"running": s.Running, "authority_epoch": 1, kReplayEpoch: s.epoch, kLastSequence: s.seq,
		"subscription_id": sub, "revision": 1, kExecGen: s.Generation,
		"pending": []any{}, "prompts": prompts,
		"info": map[string]any{"desktop_protocol": gateway.Subprotocol, "title": s.Title},
	}
}

func (g *Gateway) dispatch(c *conn, method string, p map[string]any) (any, string, func()) {
	var h func(*conn, map[string]any) (any, string, func())
	switch method {
	case gateway.MethodSessionList:
		h = g.hSessionList
	case gateway.MethodSessionCreate:
		h = g.hSessionCreate
	case gateway.MethodSessionResume:
		h = g.hSessionResume
	case gateway.MethodSessionDetach:
		h = g.hSessionDetach
	case gateway.MethodEventsSince:
		h = g.hEventsSince
	case gateway.MethodConfigSet:
		h = g.hConfigSet
	case gateway.MethodPromptSubmit:
		h = g.hPromptSubmit
	case gateway.MethodInterrupt:
		h = g.hInterrupt
	case gateway.MethodApproval:
		h = g.hApproval
	default:
		return nil, "unknown method: " + method, nil
	}
	return h(c, p)
}

func (g *Gateway) hSessionList(_ *conn, p map[string]any) (any, string, func()) {
	if !onlyKeys(p, "limit", "title", "include_hidden", "scope") {
		return nil, errInvalidParams, nil
	}
	scope := str(p, "scope")
	if scope == "" {
		scope = "live"
	}
	rows := []map[string]any{}
	for _, s := range g.sessions {
		if scope == "live" && !s.Live {
			continue
		}
		rows = append(rows, map[string]any{kSessionID: s.ID, "id": s.ID, "title": s.Title, "source": "acp",
			"started_at": s.StartedAt, "message_count": len(s.Messages), "running": s.Running})
	}
	return map[string]any{"sessions": rows, "scope": scope}, "", nil
}

func (g *Gateway) hSessionCreate(c *conn, p map[string]any) (any, string, func()) {
	if str(p, "request_id") == "" {
		return nil, errInvalidParams, nil
	}
	s := &Session{ID: "local-" + randHex()[:16], Live: true, StartedAt: float64(time.Now().Unix()), epoch: randHex(),
		prompts: map[string]gateway.Prompt{}, subs: map[*conn]string{}}
	g.sessions[s.ID] = s
	return g.snapshotLocked(c, s), "", nil
}

func (g *Gateway) hSessionResume(c *conn, p map[string]any) (any, string, func()) {
	s := g.sessions[str(p, kSessionID)]
	if s == nil {
		return nil, "not_found", nil
	}
	s.Live = true
	return g.snapshotLocked(c, s), "", nil
}

func (g *Gateway) hSessionDetach(c *conn, p map[string]any) (any, string, func()) {
	if fmt.Sprint(keys(p)) != "[session_id subscription_id]" {
		return nil, errInvalidParams, nil
	}
	s := g.sessions[str(p, kSessionID)]
	detached := s != nil && s.subs[c] == str(p, "subscription_id")
	if detached {
		delete(s.subs, c)
	}
	return map[string]any{"detached": detached}, "", nil
}

func (g *Gateway) hEventsSince(_ *conn, p map[string]any) (any, string, func()) {
	s := g.sessions[str(p, kSessionID)]
	if s == nil {
		return nil, "not_found", nil
	}
	last, _ := num(p, kLastSequence)
	if str(p, kReplayEpoch) != s.epoch || last > s.seq {
		return map[string]any{"events": []any{}, kLastSequence: s.seq, kReplayEpoch: s.epoch,
			"snapshot_required": true, "truncated": true}, "", nil
	}
	evs := []gateway.Event{}
	for _, ev := range s.ring {
		if ev.Seq > last {
			evs = append(evs, ev)
		}
	}
	return map[string]any{"events": evs, kLastSequence: s.seq, kReplayEpoch: s.epoch,
		"snapshot_required": false, "truncated": false, "count": len(evs)}, "", nil
}

func (g *Gateway) hConfigSet(_ *conn, p map[string]any) (any, string, func()) {
	s := g.sessions[str(p, kSessionID)]
	if s == nil || !onlyKeys(p, kSessionID, "profile", "key", "value") || str(p, "key") != "show_reasoning" {
		return nil, errInvalidParams, nil
	}
	switch str(p, "value") {
	case "1", "on", "true":
		s.ShowReasoning = true
	case "0", "off", "false":
		s.ShowReasoning = false
	default:
		return nil, errInvalidParams, nil
	}
	return map[string]any{"key": "show_reasoning", "value": "1", "scope": "session"}, "", nil
}

func (g *Gateway) hPromptSubmit(c *conn, p map[string]any) (any, string, func()) {
	s := g.sessions[str(p, kSessionID)]
	if s == nil || s.subs[c] == "" {
		return nil, "permission_denied", nil
	}
	if !onlyKeys(p, kSessionID, kText, "submission_id") || str(p, "submission_id") == "" {
		return nil, errInvalidParams, nil
	}
	s.Generation++
	s.Running = true
	s.Messages = append(s.Messages, map[string]any{"role": "user", kText: str(p, kText)})
	receipt := map[string]any{"admission_id": randHex(), "ref": map[string]any{kSessionID: s.ID},
		"sequence": 1, kStatus: "started", "outcome": nil, "authority_epoch": 1, kExecGen: s.Generation}
	if g.HoldTurns {
		return receipt, "", nil
	}
	text := str(p, kText)
	return receipt, "", func() { g.turn(s.ID, text) }
}

func (g *Gateway) hInterrupt(_ *conn, p map[string]any) (any, string, func()) {
	s := g.sessions[str(p, kSessionID)]
	gen, ok := num(p, kExecGen)
	if s == nil || !ok || gen != s.Generation {
		return nil, "stale_generation", nil
	}
	s.Running = false
	g.publishLocked(s, "message.complete", map[string]any{kText: "", kStatus: "interrupted"})
	return map[string]any{"execution_state": "idle", kExecGen: s.Generation}, "", nil
}

func (g *Gateway) hApproval(_ *conn, p map[string]any) (any, string, func()) {
	if fmt.Sprint(keys(p)) != "[choice execution_generation prompt_id session_id]" {
		return nil, errInvalidParams, nil
	}
	s := g.sessions[str(p, kSessionID)]
	pr, ok := s.prompts[str(p, "prompt_id")]
	if !ok {
		return map[string]any{kStatus: "already_resolved"}, "", nil
	}
	if !slices.Contains(pr.Choices, str(p, "choice")) {
		return nil, errInvalidParams, nil
	}
	delete(s.prompts, pr.PromptID)
	g.publishLocked(s, "approval.settled", map[string]any{"prompt_id": pr.PromptID, kExecGen: pr.ExecutionGeneration})
	g.publishLocked(s, "tool.complete", map[string]any{kToolID: "t-approve", kName: "terminal", kResult: str(p, "choice")})
	s.Running = false
	g.publishLocked(s, "message.complete", map[string]any{kText: "approved: " + str(p, "choice"), kStatus: "complete"})
	return map[string]any{kStatus: "resolved"}, "", nil
}

func (g *Gateway) turn(id, text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[id]
	g.publishLocked(s, "message.start", map[string]any{kText: text})
	if s.ShowReasoning {
		g.publishLocked(s, "reasoning.delta", map[string]any{kText: "thinking about "})
		g.publishLocked(s, "reasoning.delta", map[string]any{kText: text})
	}
	if text == "approve me" {
		pr := gateway.Prompt{Kind: "approval", PromptID: "ap-1", ExecutionGeneration: s.Generation, Choices: []string{"once", "deny", "session", "always"}}
		cmd := "rm -rf build"
		pr.Command = &cmd
		s.prompts[pr.PromptID] = pr
		g.publishLocked(s, "tool.start", map[string]any{kToolID: "t-approve", kName: "terminal", "args": map[string]any{"command": cmd}})
		g.publishLocked(s, "approval.request", pr)
		return
	}
	g.publishLocked(s, "tool.start", map[string]any{kToolID: "t1", kName: toolReadFile, "args": map[string]any{"path": "README.md"}})
	g.publishLocked(s, "tool.progress", map[string]any{kToolID: "t1", kName: toolReadFile, "progress": 1, "total": 2})
	g.publishLocked(s, "tool.complete", map[string]any{kToolID: "t1", kName: toolReadFile, kResult: "ok", "duration_s": 0.1})
	g.publishLocked(s, "message.delta", map[string]any{kText: "echo: "})
	g.publishLocked(s, "message.delta", map[string]any{kText: text})
	s.Running = false
	s.Messages = append(s.Messages, map[string]any{"role": "assistant", kText: "echo: " + text})
	g.publishLocked(s, "message.complete", map[string]any{kText: "echo: " + text, kStatus: "complete"})
}
