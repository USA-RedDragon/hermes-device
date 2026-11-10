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

func (c *conn) send(v any) {
	b, _ := json.Marshal(v)
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
	raw, _ := json.Marshal(payload)
	s.seq++
	gen := s.Generation
	ev := gateway.Event{Type: typ, SessionID: s.ID, Payload: raw, ReplayEpoch: s.epoch, Seq: s.seq, ExecutionGeneration: &gen}
	s.ring = append(s.ring, ev)
	for c := range s.subs {
		c.send(map[string]any{"jsonrpc": "2.0", "method": "event", "params": ev})
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
	http.SetCookie(w, &http.Cookie{Name: "hermes_session_at", Value: v, Path: "/", HttpOnly: true})
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
	_ = json.NewEncoder(w).Encode(map[string]any{"ticket": t, "ttl_seconds": 30})
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
	c.send(map[string]any{"jsonrpc": "2.0", "method": "event", "params": map[string]any{
		"type": gateway.ReadyEvent, "payload": map[string]any{"session_authority": true, "replay_epoch": "x"}}})
	for {
		_, data, err := ws.Read(context.Background())
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
			c.send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32000, "message": rerr}})
		} else {
			c.send(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
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
		"session_id": s.ID, "stored_session_id": s.ID, "messages": msgs, "message_count": len(msgs),
		"running": s.Running, "authority_epoch": 1, "replay_epoch": s.epoch, "last_sequence": s.seq,
		"subscription_id": sub, "revision": 1, "execution_generation": s.Generation,
		"pending": []any{}, "prompts": prompts,
		"info": map[string]any{"desktop_protocol": gateway.Subprotocol, "title": s.Title},
	}
}

func (g *Gateway) dispatch(c *conn, method string, p map[string]any) (any, string, func()) {
	switch method {
	case gateway.MethodSessionList:
		if !onlyKeys(p, "limit", "title", "include_hidden", "scope") {
			return nil, "invalid_params", nil
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
			rows = append(rows, map[string]any{"session_id": s.ID, "id": s.ID, "title": s.Title, "source": "acp",
				"started_at": s.StartedAt, "message_count": len(s.Messages), "running": s.Running})
		}
		return map[string]any{"sessions": rows, "scope": scope}, "", nil
	case gateway.MethodSessionCreate:
		if str(p, "request_id") == "" {
			return nil, "invalid_params", nil
		}
		s := &Session{ID: "local-" + randHex()[:16], Live: true, StartedAt: float64(time.Now().Unix()), epoch: randHex(),
			prompts: map[string]gateway.Prompt{}, subs: map[*conn]string{}}
		g.sessions[s.ID] = s
		return g.snapshotLocked(c, s), "", nil
	case gateway.MethodSessionResume:
		s := g.sessions[str(p, "session_id")]
		if s == nil {
			return nil, "not_found", nil
		}
		s.Live = true
		return g.snapshotLocked(c, s), "", nil
	case gateway.MethodSessionDetach:
		if fmt.Sprint(keys(p)) != "[session_id subscription_id]" {
			return nil, "invalid_params", nil
		}
		s := g.sessions[str(p, "session_id")]
		detached := s != nil && s.subs[c] == str(p, "subscription_id")
		if detached {
			delete(s.subs, c)
		}
		return map[string]any{"detached": detached}, "", nil
	case gateway.MethodEventsSince:
		s := g.sessions[str(p, "session_id")]
		if s == nil {
			return nil, "not_found", nil
		}
		last, _ := num(p, "last_sequence")
		if str(p, "replay_epoch") != s.epoch || last > s.seq {
			return map[string]any{"events": []any{}, "last_sequence": s.seq, "replay_epoch": s.epoch,
				"snapshot_required": true, "truncated": true}, "", nil
		}
		evs := []gateway.Event{}
		for _, ev := range s.ring {
			if ev.Seq > last {
				evs = append(evs, ev)
			}
		}
		return map[string]any{"events": evs, "last_sequence": s.seq, "replay_epoch": s.epoch,
			"snapshot_required": false, "truncated": false, "count": len(evs)}, "", nil
	case gateway.MethodConfigSet:
		s := g.sessions[str(p, "session_id")]
		if s == nil || !onlyKeys(p, "session_id", "profile", "key", "value") || str(p, "key") != "show_reasoning" {
			return nil, "invalid_params", nil
		}
		switch str(p, "value") {
		case "1", "on", "true":
			s.ShowReasoning = true
		case "0", "off", "false":
			s.ShowReasoning = false
		default:
			return nil, "invalid_params", nil
		}
		return map[string]any{"key": "show_reasoning", "value": "1", "scope": "session"}, "", nil
	case gateway.MethodPromptSubmit:
		s := g.sessions[str(p, "session_id")]
		if s == nil || s.subs[c] == "" {
			return nil, "permission_denied", nil
		}
		if !onlyKeys(p, "session_id", "text", "submission_id") || str(p, "submission_id") == "" {
			return nil, "invalid_params", nil
		}
		s.Generation++
		s.Running = true
		s.Messages = append(s.Messages, map[string]any{"role": "user", "text": str(p, "text")})
		receipt := map[string]any{"admission_id": randHex(), "ref": map[string]any{"session_id": s.ID},
			"sequence": 1, "status": "started", "outcome": nil, "authority_epoch": 1, "execution_generation": s.Generation}
		if g.HoldTurns {
			return receipt, "", nil
		}
		text := str(p, "text")
		return receipt, "", func() { g.turn(s.ID, text) }
	case gateway.MethodInterrupt:
		s := g.sessions[str(p, "session_id")]
		gen, ok := num(p, "execution_generation")
		if s == nil || !ok || gen != s.Generation {
			return nil, "stale_generation", nil
		}
		s.Running = false
		g.publishLocked(s, "message.complete", map[string]any{"text": "", "status": "interrupted"})
		return map[string]any{"execution_state": "idle", "execution_generation": s.Generation}, "", nil
	case gateway.MethodApproval:
		if fmt.Sprint(keys(p)) != "[choice execution_generation prompt_id session_id]" {
			return nil, "invalid_params", nil
		}
		s := g.sessions[str(p, "session_id")]
		pr, ok := s.prompts[str(p, "prompt_id")]
		if !ok {
			return map[string]any{"status": "already_resolved"}, "", nil
		}
		if !slices.Contains(pr.Choices, str(p, "choice")) {
			return nil, "invalid_params", nil
		}
		delete(s.prompts, pr.PromptID)
		g.publishLocked(s, "approval.settled", map[string]any{"prompt_id": pr.PromptID, "execution_generation": pr.ExecutionGeneration})
		g.publishLocked(s, "tool.complete", map[string]any{"tool_id": "t-approve", "name": "terminal", "result": str(p, "choice")})
		s.Running = false
		g.publishLocked(s, "message.complete", map[string]any{"text": "approved: " + str(p, "choice"), "status": "complete"})
		return map[string]any{"status": "resolved"}, "", nil
	}
	return nil, "unknown method: " + method, nil
}

func (g *Gateway) turn(id, text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[id]
	g.publishLocked(s, "message.start", map[string]any{"text": text})
	if s.ShowReasoning {
		g.publishLocked(s, "reasoning.delta", map[string]any{"text": "thinking about "})
		g.publishLocked(s, "reasoning.delta", map[string]any{"text": text})
	}
	if text == "approve me" {
		pr := gateway.Prompt{Kind: "approval", PromptID: "ap-1", ExecutionGeneration: s.Generation, Choices: []string{"once", "deny", "session", "always"}}
		cmd := "rm -rf build"
		pr.Command = &cmd
		s.prompts[pr.PromptID] = pr
		g.publishLocked(s, "tool.start", map[string]any{"tool_id": "t-approve", "name": "terminal", "args": map[string]any{"command": cmd}})
		g.publishLocked(s, "approval.request", pr)
		return
	}
	g.publishLocked(s, "tool.start", map[string]any{"tool_id": "t1", "name": "read_file", "args": map[string]any{"path": "README.md"}})
	g.publishLocked(s, "tool.progress", map[string]any{"tool_id": "t1", "name": "read_file", "progress": 1, "total": 2})
	g.publishLocked(s, "tool.complete", map[string]any{"tool_id": "t1", "name": "read_file", "result": "ok", "duration_s": 0.1})
	g.publishLocked(s, "message.delta", map[string]any{"text": "echo: "})
	g.publishLocked(s, "message.delta", map[string]any{"text": text})
	s.Running = false
	s.Messages = append(s.Messages, map[string]any{"role": "assistant", "text": "echo: " + text})
	g.publishLocked(s, "message.complete", map[string]any{"text": "echo: " + text, "status": "complete"})
}
