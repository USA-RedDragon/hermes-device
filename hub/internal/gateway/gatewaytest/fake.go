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
	"net"
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
	kParams          = "params"
	kToolID          = "tool_id"
	kText            = "text"
	kStatus          = "status"
	kName            = "name"
	kResult          = "result"
	kReplayEpoch     = "replay_epoch"
	kLastSequence    = "last_sequence"
	kExecGen         = "execution_generation"
	errInvalidParams = "invalid_params"
	errInternal      = "internal error"
	kPayload         = "payload"
	kModel           = "model"
	kReasoning       = "reasoning"
	kTotal           = "total"
	kScope           = "scope"
	kSession         = "session"
	kTerminal        = "terminal"
	kArgs            = "args"
	kIsError         = "is_error"
	kDuration        = "duration_s"
	kLocal           = "local"
	kCode            = "code"
	kProvider        = "provider"
	kSmall           = "small"
	kMessage         = "message"
	kRevision        = "revision"
	kOperation       = "operation"
	kTitle           = "title"
	jsonRPCVersion   = "2.0"
	toolReadFile     = "read_file"
)

// Call is one request the fake received.
type Call struct {
	Method string
	Params map[string]any
	// Raw is the whole message as the gateway got it.
	Raw json.RawMessage
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
	// Pending is the snapshot's pending list: queued inputs and lost (unknown) turns.
	Pending []gateway.PendingAdmission
	// Revision is the runtime revision session.mutate is fenced on. Turns
	// and settings changes move it, as in Hermes.
	Revision int64
	Archived bool
	Model    string
	Provider string
	CWD      string
	// RowCWD is the row_cwd session.create named.
	RowCWD string
	// RequestID is the request_id session.create named.
	RequestID string
	// Source is the surface that started the session; "" lists as "acp".
	Source string
	// Effort is the reasoning effort; "" is the profile default.
	Effort string

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

	mux      *http.ServeMux
	mu       sync.Mutex
	sessions map[string]*Session
	tickets  map[string]bool
	cookies  map[string]bool
	conns    map[*conn]bool
	calls    []Call
	logins   int
	// HoldTurns stops prompt.submit from running its scripted turn.
	HoldTurns bool
	// Providers is what model.options reports.
	Providers []gateway.ModelProvider
	// ConfirmModels need a confirmation token before session.mutate
	// commits them.
	ConfirmModels []string

	notReady   bool
	loseAnswer bool
	crash      bool
	refuse     map[string]string
	down       *gatedListener
	// submissions are the admissions by session and submission_id, which
	// outlive a restart as Hermes' session_admissions rows do.
	submissions map[string]submission
}

type submission struct {
	text    string
	receipt map[string]any
}

// gatedListener is the fake's listener. While down it accepts and at once
// closes every connection, as a host with nothing listening on the port.
type gatedListener struct {
	net.Listener
	mu   sync.Mutex
	shut bool
}

func (l *gatedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		l.mu.Lock()
		shut := l.shut
		l.mu.Unlock()
		if !shut {
			return c, nil
		}
		_ = c.Close()
	}
}

func (l *gatedListener) set(shut bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.shut = shut
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
	yes, no := true, false
	g := &Gateway{
		Username: username, Password: password,
		sessions: map[string]*Session{}, tickets: map[string]bool{},
		cookies: map[string]bool{}, conns: map[*conn]bool{},
		Providers: []gateway.ModelProvider{
			{Slug: kLocal, Name: "Local", Models: []string{kSmall, "big", "gone"}, Authenticated: &yes,
				UnavailableModels: []string{"gone"},
				Capabilities:      map[string]gateway.ModelCapabilities{kSmall: {Reasoning: false}, "big": {Reasoning: true}}},
			{Slug: "cloud", Name: "Cloud", Models: []string{"remote"}, Authenticated: &no},
		},
	}
	mux := http.NewServeMux()
	g.mux = mux
	mux.HandleFunc("POST /auth/password-login", g.login)
	mux.HandleFunc("POST /api/webapp/socket-ticket", g.ticket)
	mux.HandleFunc("GET /api/ws", g.ws)
	g.submissions = map[string]submission{}
	g.Server = httptest.NewUnstartedServer(g.notReadyGate(mux))
	g.down = &gatedListener{Listener: g.Server.Listener}
	g.Server.Listener = g.down
	g.Server.Start()
	return g
}

// notReadyGate answers every HTTP request with 503 gateway_not_ready while
// the gateway restarts, as gateway/run_api.py does. A WebSocket upgrade
// there is closed with 1013 before it is accepted, which uvicorn sends as
// HTTP 403; the hub never gets that far, since its ticket mint fails first.
func (g *Gateway) notReadyGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		notReady := g.notReady
		g.mu.Unlock()
		switch {
		case !notReady:
			next.ServeHTTP(w, r)
		case r.Header.Get("Upgrade") != "":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"gateway_not_ready","state":"draining"}`))
		}
	})
}

// BeginRestart makes the gateway drain for a restart: HTTP requests get
// 503 gateway_not_ready, and each open socket is closed with 1013 when its
// next frame arrives, which goes unanswered.
func (g *Gateway) BeginRestart() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.notReady = true
}

// LoseNextSubmitAnswer makes the next prompt.submit admit its input and
// run its turn, but drop the connection before the answer goes out.
func (g *Gateway) LoseNextSubmitAnswer() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loseAnswer = true
}

// CrashOnNextSubmit makes the next prompt.submit admit its input, then the
// gateway stop before the answer goes out.
func (g *Gateway) CrashOnNextSubmit() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.loseAnswer, g.crash = true, true
}

// RefuseSubmits makes prompt.submit on a session fail with reason, as
// Hermes refuses runtime_draining for a profile that is still starting.
// An empty reason ends it.
func (g *Gateway) RefuseSubmits(sessionID, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refuse == nil {
		g.refuse = map[string]string{}
	}
	g.refuse[sessionID] = reason
}

// Stop makes the gateway's process exit: every connection closes and new
// ones are cut at once.
func (g *Gateway) Stop() {
	g.down.set(true)
	g.DropConnections()
	g.Server.CloseClientConnections()
}

// FinishRestart brings the gateway back as a new process: it is ready,
// every session has a new replay epoch and an empty replay ring, and no
// turn runs. Admissions survive, so a retried submission_id still answers
// its first receipt.
func (g *Gateway) FinishRestart() {
	g.mu.Lock()
	g.notReady = false
	for _, s := range g.sessions {
		s.epoch, s.ring, s.Running = randHex(), nil, false
		s.subs = map[*conn]string{}
	}
	g.mu.Unlock()
	g.down.set(false)
}

// Handle serves pattern on the fake's listener, as a dashboard plugin's
// route. With signedIn, a request without a dashboard session cookie gets
// 401 as on Hermes' /api/* routes.
func (g *Gateway) Handle(pattern string, h http.Handler, signedIn bool) {
	if !signedIn {
		g.mux.Handle(pattern, h)
		return
	}
	g.mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ck, err := r.Cookie("hermes_session_at")
		g.mu.Lock()
		ok := err == nil && g.cookies[ck.Value]
		g.mu.Unlock()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"Unauthorized"}`))
			return
		}
		h.ServeHTTP(w, r)
	}))
}

// SessionIDs lists the ids of every session.
func (g *Gateway) SessionIDs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, 0, len(g.sessions))
	for id := range g.sessions {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
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
	if s.Model == "" {
		s.Model, s.Provider = kSmall, kLocal
	}
	g.sessions[s.ID] = s
}

// Session returns a copy of a session's state, or false once it is deleted.
func (g *Gateway) Session(id string) (Session, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.sessions[id]
	if !ok {
		return Session{}, false
	}
	return *s, true
}

// Submits counts the prompt.submit calls that admitted a new input, so
// not the retries Hermes answered from an earlier admission.
func (g *Gateway) Submits() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.submissions)
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
		c.send(map[string]any{kJSONRPC: jsonRPCVersion, kMethod: kEvent, kParams: map[string]any{
			"type": gateway.ReplayGapEvent, kSessionID: s.ID,
			kPayload: map[string]any{kReplayEpoch: s.epoch, "latest_seq": s.seq}}})
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
	g.publishAdmissionLocked(s, typ, "", payload)
}

// publishAdmissionLocked publishes an event that names the admission it
// belongs to on the frame, as Hermes does for turn events.
func (g *Gateway) publishAdmissionLocked(s *Session, typ, admission string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	s.seq++
	gen := s.Generation
	ev := gateway.Event{Type: typ, SessionID: s.ID, Payload: raw, ReplayEpoch: s.epoch, Seq: s.seq, ExecutionGeneration: &gen,
		AdmissionID: admission}
	s.ring = append(s.ring, ev)
	for c := range s.subs {
		c.send(map[string]any{kJSONRPC: jsonRPCVersion, kMethod: kEvent, kParams: ev})
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
	if r.URL.Query().Get("socket") != "gateway" {
		http.Error(w, `{"detail":"Unknown socket"}`, http.StatusBadRequest)
		return
	}
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
	if r.Header.Get("Origin") != "http://"+r.Host || r.URL.Query().Get("native_dial") != "browser" {
		http.Error(w, "native ticket dial from a non-loopback peer", http.StatusForbidden)
		return
	}
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
	c.send(map[string]any{kJSONRPC: jsonRPCVersion, kMethod: kEvent, kParams: map[string]any{
		"type": gateway.ReadyEvent, kPayload: map[string]any{"session_authority": true, kReplayEpoch: "x"}}})
	for {
		_, data, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(data, &req) != nil {
			continue
		}
		g.mu.Lock()
		if g.notReady {
			g.mu.Unlock()
			_ = ws.Close(websocket.StatusCode(1013), "")
			return
		}
		g.calls = append(g.calls, Call{Method: req.Method, Params: req.Params, Raw: append(json.RawMessage(nil), data...)})
		var members map[string]json.RawMessage
		_ = json.Unmarshal(data, &members)
		if members["method"] == nil {
			g.mu.Unlock()
			continue
		}
		result, rerr, after := g.dispatch(c, req.Method, req.Params)
		var errFrame map[string]any
		if rerr != "" {
			errFrame = g.refusalLocked(req.Method, req.Params, rerr)
		}
		lose := g.loseAnswer && req.Method == gateway.MethodPromptSubmit && rerr == ""
		crash := lose && g.crash
		if lose {
			g.loseAnswer, g.crash = false, false
		}
		g.mu.Unlock()
		if lose {
			if after != nil {
				go after()
			}
			if crash {
				go g.Stop()
			}
			_ = ws.CloseNow()
			return
		}
		if errFrame != nil {
			c.send(map[string]any{kJSONRPC: jsonRPCVersion, "id": req.ID, "error": errFrame})
		} else {
			c.send(map[string]any{kJSONRPC: jsonRPCVersion, "id": req.ID, kResult: result})
		}
		if after != nil {
			go after()
		}
	}
}

// refusalLocked builds an error frame as gateway/session_controls.py does:
// code 4001, the reason as the message and in data, and for a stale
// session.mutate the fresh fences.
func (g *Gateway) refusalLocked(method string, p map[string]any, reason string) map[string]any {
	switch {
	case strings.HasPrefix(reason, "unknown method"):
		return map[string]any{kCode: -32601, kMessage: reason}
	case reason == errInternal:
		return map[string]any{kCode: -32603, kMessage: reason}
	}
	data := map[string]any{"reason": reason}
	if s := g.sessions[str(p, kSessionID)]; s != nil && method == gateway.MethodMutate &&
		(reason == gateway.ReasonRevisionConflict || reason == gateway.ReasonStaleGeneration) {
		data["revision"], data[kExecGen] = s.Revision, s.Generation
	}
	return map[string]any{kCode: 4001, kMessage: reason, "data": data}
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
	pending := s.Pending
	if pending == nil {
		pending = []gateway.PendingAdmission{}
	}
	return map[string]any{
		kSessionID: s.ID, "stored_session_id": s.ID, "messages": msgs, "message_count": len(msgs),
		"running": s.Running, "authority_epoch": 1, kReplayEpoch: s.epoch, kLastSequence: s.seq,
		"subscription_id": sub, kRevision: s.Revision, kExecGen: s.Generation,
		"pending": pending, "prompts": prompts,
		"info": g.infoLocked(s, true),
	}
}

// infoLocked is a session.info payload (gateway/session_local.py
// local_session_info); full adds the inventory a snapshot carries.
func (g *Gateway) infoLocked(s *Session, full bool) map[string]any {
	var title any
	if s.Title != "" {
		title = s.Title
	}
	info := map[string]any{"desktop_protocol": gateway.Subprotocol, kTitle: title, "source": "gui", kModel: s.Model,
		"cwd": s.CWD, "reasoning_effort": s.Effort, "reasoning_effort_wire": s.Effort, "fast": false, "lazy": false,
		"started_at": s.StartedAt}
	if full {
		info[kProvider], info["context_length"] = s.Provider, 131072
		info["usage"] = map[string]any{kModel: s.Model, "input": 0, "output": 0, kReasoning: 0, "prompt": 0,
			"completion": 0, kTotal: 0, "calls": 0}
	}
	return info
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
	case gateway.MethodPromptCancel:
		h = g.hPromptCancel
	case gateway.MethodResolveLost:
		h = g.hResolveLost
	case gateway.MethodMutate:
		h = g.hMutate
	case gateway.MethodModelOptions:
		h = g.hModelOptions
	default:
		return nil, "unknown method: " + method, nil
	}
	return h(c, p)
}

func (g *Gateway) hSessionList(_ *conn, p map[string]any) (any, string, func()) {
	if !onlyKeys(p, "limit", "title", "include_hidden", kScope) {
		return nil, errInvalidParams, nil
	}
	scope := str(p, kScope)
	if scope == "" {
		scope = "live"
	}
	hidden, _ := p["include_hidden"].(bool)
	rows := []map[string]any{}
	for _, s := range g.sessions {
		if scope == "live" && !s.Live || s.Archived && (scope == "stored" || !hidden) {
			continue
		}
		source := s.Source
		if source == "" {
			source = gateway.SourceACP
		}
		rows = append(rows, map[string]any{kSessionID: s.ID, "id": s.ID, kTitle: s.Title, "source": source,
			"started_at": s.StartedAt, "message_count": len(s.Messages), "running": s.Running, kModel: s.Model})
	}
	return map[string]any{"sessions": rows, kScope: scope}, "", nil
}

func (g *Gateway) hSessionCreate(c *conn, p map[string]any) (any, string, func()) {
	if str(p, "request_id") == "" {
		return nil, errInvalidParams, nil
	}
	for _, s := range g.sessions {
		if s.RequestID == str(p, "request_id") {
			return g.snapshotLocked(c, s), "", nil
		}
	}
	s := &Session{ID: "local-" + randHex()[:16], Live: true, StartedAt: float64(time.Now().Unix()), epoch: randHex(),
		prompts: map[string]gateway.Prompt{}, subs: map[*conn]string{}, Model: kSmall, Provider: kLocal,
		CWD: str(p, "cwd"), RowCWD: str(p, "row_cwd"), RequestID: str(p, "request_id"), Source: str(p, "source")}
	g.sessions[s.ID] = s
	return g.snapshotLocked(c, s), "", nil
}

func (g *Gateway) hSessionResume(c *conn, p map[string]any) (any, string, func()) {
	s := g.sessions[str(p, kSessionID)]
	if t, ok := p[kTitle].(string); ok {
		s = nil
		for _, x := range g.sessions {
			if x.Title == t {
				s = x
			}
		}
	}
	if _, ok := p["latest"]; ok {
		s = nil
		for _, x := range g.sessions {
			if s == nil || x.StartedAt > s.StartedAt {
				s = x
			}
		}
	}
	if s == nil {
		return nil, gateway.ReasonNotFound, nil
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
		return nil, gateway.ReasonNotFound, nil
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
	if s != nil && str(p, "key") == gateway.ReasoningKey {
		return g.setReasoning(s, p)
	}
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
	return map[string]any{"key": "show_reasoning", "value": "1", kScope: kSession}, "", nil
}

func (g *Gateway) hPromptSubmit(c *conn, p map[string]any) (any, string, func()) {
	s := g.sessions[str(p, kSessionID)]
	if s == nil || s.subs[c] == "" {
		return nil, "permission_denied", nil
	}
	if !onlyKeys(p, kSessionID, kText, "submission_id") || str(p, "submission_id") == "" {
		return nil, errInvalidParams, nil
	}
	if r := g.refuse[s.ID]; r != "" {
		return nil, r, nil
	}
	key := s.ID + "\x00" + str(p, "submission_id")
	if old, ok := g.submissions[key]; ok {
		if old.text != str(p, kText) {
			return nil, "admission_conflict", nil
		}
		return old.receipt, "", nil
	}
	s.Generation++
	s.Running = true
	s.Messages = append(s.Messages, map[string]any{"role": "user", kText: str(p, kText)})
	admission := randHex()
	receipt := map[string]any{"admission_id": admission, "ref": map[string]any{kSessionID: s.ID},
		"sequence": 1, kStatus: "started", "outcome": nil, "authority_epoch": 1, kExecGen: s.Generation}
	g.submissions[key] = submission{text: str(p, kText), receipt: receipt}
	if g.HoldTurns {
		return receipt, "", nil
	}
	text := str(p, kText)
	return receipt, "", func() { g.turn(s.ID, admission, text) }
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
	g.publishLocked(s, "tool.complete", map[string]any{kToolID: "t-approve", kName: kTerminal, kResult: str(p, "choice")})
	s.Running = false
	g.publishLocked(s, "message.complete", map[string]any{kText: "approved: " + str(p, "choice"), kStatus: "complete"})
	return map[string]any{kStatus: "resolved"}, "", nil
}

func (g *Gateway) turn(id, admission, text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[id]
	// Like Hermes: session.info lists the input with its text, then
	// message.start names the admission with an empty payload.
	s.Revision++
	g.publishLocked(s, gateway.InfoEvent, map[string]any{"pending": []gateway.PendingAdmission{
		{AdmissionID: admission, Status: "started", Text: text}}, kRevision: s.Revision, kExecGen: s.Generation})
	g.publishAdmissionLocked(s, "message.start", admission, map[string]any{})
	if s.ShowReasoning {
		g.publishLocked(s, "reasoning.delta", map[string]any{kText: "thinking about "})
		g.publishLocked(s, "reasoning.delta", map[string]any{kText: text})
	}
	if text == "approve me" {
		pr := gateway.Prompt{Kind: "approval", PromptID: "ap-1", ExecutionGeneration: s.Generation, Choices: []string{"once", "deny", kSession, "always"}}
		cmd := "rm -rf build"
		pr.Command = &cmd
		s.prompts[pr.PromptID] = pr
		g.publishLocked(s, "tool.start", map[string]any{kToolID: "t-approve", kName: kTerminal, kArgs: map[string]any{"command": cmd}})
		g.publishLocked(s, "approval.request", pr)
		return
	}
	g.publishLocked(s, "tool.start", map[string]any{kToolID: "t1", kName: toolReadFile, kArgs: map[string]any{"path": "README.md"}})
	g.publishLocked(s, "tool.progress", map[string]any{kToolID: "t1", kName: toolReadFile, "progress": 1, kTotal: 2})
	switch text {
	case "edit":
		g.publishLocked(s, "tool.start", map[string]any{kToolID: "t-edit", kName: "patch", kArgs: map[string]any{"path": "a.go"}})
		g.publishLocked(s, "tool.complete", map[string]any{kToolID: "t-edit", kName: "patch", kIsError: false,
			kResult: `{"success": true}`, kDuration: 0.2,
			"inline_diff": "  review diff\n\x1b[38;2;180;160;255ma/a.go \u2192 b/a.go\x1b[0m\n\x1b[38;2;120;120;140m@@ -1 +1 @@\x1b[0m\n" +
				"\x1b[38;2;255;255;255;48;2;120;20;20m-old\x1b[0m\n\x1b[38;2;255;255;255;48;2;20;90;20m+new\x1b[0m"})
	case "run":
		g.publishLocked(s, "tool.start", map[string]any{kToolID: "t-run", kName: kTerminal, kArgs: map[string]any{"command": "ls"}})
		g.publishLocked(s, "tool.complete", map[string]any{kToolID: "t-run", kName: kTerminal, kIsError: false,
			kResult: `{"output": "a.go\nb.go", "exit_code": 0, "error": null}`, kDuration: 1.5})
	}
	g.publishLocked(s, "tool.complete", map[string]any{kToolID: "t1", kName: toolReadFile, kResult: "ok", kDuration: 0.1, kIsError: false})
	g.publishLocked(s, "message.delta", map[string]any{kText: "echo: "})
	g.publishLocked(s, "message.delta", map[string]any{kText: text})
	s.Running = false
	s.Messages = append(s.Messages, map[string]any{"role": "assistant", kText: "echo: " + text})
	g.publishLocked(s, "message.complete", map[string]any{kText: "echo: " + text, kStatus: "complete",
		"usage": map[string]any{kModel: s.Model, "input": 9000, "output": 120, kReasoning: 0, "prompt": 9000, "completion": 120,
			kTotal: 9120, "calls": 1, "context_used": 9000, "context_max": 131072, "context_percent": 7,
			"context_source": "provider_usage", "context_estimated": false, "avg_tps": 42.5}})
}

// hPromptCancel retires a queued input, as Hermes refuses for any other status.
func (g *Gateway) hPromptCancel(_ *conn, p map[string]any) (any, string, func()) {
	if fmt.Sprint(keys(p)) != "[admission_id session_id]" {
		return nil, errInvalidParams, nil
	}
	return g.dropPending(str(p, kSessionID), str(p, "admission_id"), gateway.AdmissionQueued, nil)
}

// hResolveLost settles a turn lost in a restart; it needs that turn's generation.
func (g *Gateway) hResolveLost(_ *conn, p map[string]any) (any, string, func()) {
	if fmt.Sprint(keys(p)) != "[admission_id execution_generation session_id]" {
		return nil, errInvalidParams, nil
	}
	gen, _ := num(p, kExecGen)
	return g.dropPending(str(p, kSessionID), str(p, "admission_id"), gateway.AdmissionUnknown, &gen)
}

func (g *Gateway) dropPending(sid, id, status string, gen *int64) (any, string, func()) {
	s := g.sessions[sid]
	if s == nil {
		return nil, gateway.ReasonNotFound, nil
	}
	for i, a := range s.Pending {
		if a.AdmissionID != id {
			continue
		}
		if a.Status != status || (gen != nil && (a.ExecutionGeneration == nil || *a.ExecutionGeneration != *gen)) {
			return nil, "stale_generation", nil
		}
		s.Pending = slices.Delete(s.Pending, i, i+1)
		return map[string]any{"admission_id": id, kStatus: kTerminal, "outcome": "interrupted"}, "", nil
	}
	return nil, gateway.ReasonNotFound, nil
}
