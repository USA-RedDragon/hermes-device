// Package gatewaytest is an in-process fake of the Hermes v0.23.1 gateway
// Webapp listener: password login, WebSocket tickets and the subset of the
// canonical session contract the hub uses. Its checks on params follow
// gateway/session_controls.py.
package gatewaytest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/coder/websocket"
)

const (
	kSessionID       = "session_id"
	kPending         = "pending"
	kRole            = "role"
	kAssistant       = "assistant"
	kSubmission      = "submission_id"
	kComplete        = "complete"
	kJSONRPC         = "jsonrpc"
	kEvent           = "event"
	kMethod          = "method"
	kParams          = "params"
	kToolID          = "tool_id"
	kText            = "text"
	kInflightSeq     = "sequence"
	kInflightEvents  = "events"
	kMessageComplete = "message.complete"
	kStatus          = "status"
	kOutcome         = "outcome"
	kName            = "name"
	kPath            = "path"
	kResult          = "result"
	kReplayEpoch     = "replay_epoch"
	kLastSequence    = "last_sequence"
	kExecGen         = "execution_generation"
	errInvalidParams = "invalid_params"
	errDenied        = "permission_denied"
	errInternal      = "internal error"
	kPayload         = "payload"
	kModel           = "model"
	kReasoning       = "reasoning"
	kTotal           = "total"
	kScope           = "scope"
	kKey             = "key"
	kValue           = "value"
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
	// Yolo is the session's approval bypass, as config.get yolo reads it.
	Yolo bool
	// Pending is the snapshot's pending list: queued inputs and lost (unknown) turns.
	Pending []gateway.PendingAdmission
	// turn is the admission of the running turn, transfers the queued
	// inputs prompt.steer_queued handed to it.
	turn, turnInput string
	transfers       []string
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
	// Tools are the session's tools by toolset, as session.info lists them.
	Tools map[string][]string
	// Attachments are the attachments of each prompt.submit, in order.
	Attachments [][]gateway.Attachment
	// Prompts are the prompts pending when the session is added.
	Prompts []gateway.Prompt
	// Subagents are the subagents the session runs, as subagent.list
	// lists them.
	Subagents []gateway.SubagentRow
	// children are the stored transcripts of its subagents, by id.
	children map[string]*child
	// Processes are the session's background processes, as process.list
	// lists them.
	Processes []gateway.ProcessRow
	// LastActiveAt is when the session last changed, which session.list
	// reports with picker; 0 reports none.
	LastActiveAt float64
	// Parent is the session this one was branched from.
	Parent string

	epoch string
	seq   int64
	ring  []gateway.Event
	// evicted is the seq of the newest event the replay ring dropped.
	evicted int64
	// reply is the running reply's text in its current segment.
	reply string
	// turnEvents are the running turn's events, for InflightTurn;
	// turnEnded says the last of them ended it.
	turnEvents []gateway.Event
	turnEnded  bool
	prompts    map[string]gateway.Prompt
	subs       map[*conn]string
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
	// ReplayMax bounds each session's replay ring as Hermes does
	// (tui_gateway/event_replay.py _REPLAY_BUFFER_MAX, 512): older events
	// are dropped, and session.events.since from before them answers
	// snapshot_required. 0 keeps every event.
	ReplayMax int
	// InflightReply makes session.resume of a running session carry the
	// reply's text so far, as Talaria Hermes patch 0057 does.
	InflightReply bool
	// InflightTurn makes session.resume of a running session carry the
	// running turn's events so far, as Talaria Hermes patch 0063 does.
	InflightTurn bool
	// ResendOnResume sends the last that many ring events again after a
	// resume of a running session: frames the snapshot already covers, as a
	// frame published while the subscription opens can arrive.
	ResendOnResume int
	// Providers is what model.options reports.
	Providers []gateway.ModelProvider
	// ConfirmModels need a confirmation token before session.mutate
	// commits them.
	ConfirmModels []string

	notReady   bool
	loseAnswer bool
	crash      bool
	refuse     map[string]string
	// drainingCalls answers that many more calls of a method with
	// runtime_draining (RefuseWhileDraining).
	drainingCalls map[string]int
	// refusedCalls answers every call of a method with a reason.
	refusedCalls map[string]string
	down         *gatedListener
	// submissions are the admissions by session and submission_id, which
	// outlive a restart as Hermes' session_admissions rows do.
	submissions map[string]submission
	files       fileStore
	paused      bool
	steers      []Steer
	caps        []string
	audiences   map[string][]string
	// QueueWhileRunning makes prompt.submit queue a prompt behind the
	// running turn, as Hermes does, instead of running it at once.
	QueueWhileRunning bool
	steerScript       []string
	steered           []string
	orphanBranches    bool
	commands          []string
	compressed        []string
	// stalls hold the next call of a method on its connection (StallNext).
	stalls map[string]chan struct{}
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

// send writes v, giving the write its own deadline within ctx.
func (c *conn) send(ctx context.Context, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
	g.Handle("POST /api/files/ticket", http.HandlerFunc(g.fileTicket), true)
	g.Handle("GET /api/fs/download", http.HandlerFunc(g.fileDownload), true)
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

// RefuseWhileDraining makes the next n calls of method fail with
// runtime_draining, as a Hermes gateway in its restart wait refuses a verb
// it does not serve there.
func (g *Gateway) RefuseWhileDraining(method string, n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.drainingCalls == nil {
		g.drainingCalls = map[string]int{}
	}
	g.drainingCalls[method] = n
}

// RefuseCalls makes every call of method fail with reason, as Hermes
// refuses session.create with no_model_configured. An empty reason ends it.
func (g *Gateway) RefuseCalls(method, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refusedCalls == nil {
		g.refusedCalls = map[string]string{}
	}
	g.refusedCalls[method] = reason
}

// StallNext makes the next call of method hang on its connection until
// release is called, as Hermes does with a control request it is stuck
// on: the gateway answers the requests of one connection in order, so every
// later request on that connection waits too, while other connections are
// answered at once. Once released the held call is answered.
func (g *Gateway) StallNext(method string) (release func()) {
	ch := make(chan struct{})
	g.mu.Lock()
	if g.stalls == nil {
		g.stalls = map[string]chan struct{}{}
	}
	g.stalls[method] = ch
	g.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
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
	for _, p := range s.Prompts {
		s.prompts[p.PromptID] = p
	}
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

// RestartConnections closes every open WebSocket the way a gateway that
// goes away for a restart does: close 1012 (service restart).
func (g *Gateway) RestartConnections() {
	g.mu.Lock()
	conns := make([]*conn, 0, len(g.conns))
	for c := range g.conns {
		conns = append(conns, c)
	}
	g.mu.Unlock()
	for _, c := range conns {
		_ = c.ws.Close(websocket.StatusServiceRestart, "")
	}
}

// ConnCount is the number of open WebSockets.
func (g *Gateway) ConnCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.conns)
}

// Ask makes a session wait for an approval, as a turn's tool call does.
func (g *Gateway) Ask(sessionID string, pr gateway.Prompt) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[sessionID]
	if s.prompts == nil {
		s.prompts = map[string]gateway.Prompt{}
	}
	s.prompts[pr.PromptID] = pr
	g.publishLocked(s, "approval.request", pr)
}

// AskSilently makes a session hold an approval that no event announced, as
// a gateway that took over from a restarted one does for a waiter it
// resumed: only a snapshot lists it.
func (g *Gateway) AskSilently(sessionID string, pr gateway.Prompt) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessions[sessionID].prompts[pr.PromptID] = pr
}

// Withdraw drops pending prompts with no event and no answer, as a
// restart does to the threads that waited on them.
func (g *Gateway) Withdraw(sessionID string, ids ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, id := range ids {
		delete(g.sessions[sessionID].prompts, id)
	}
}

// ServerRequest sends a server request to every open WebSocket, as
// tui_gateway/server_requests.py does to a client that answers them.
func (g *Gateway) ServerRequest(id, method string, params any) {
	g.mu.Lock()
	conns := make([]*conn, 0, len(g.conns))
	for c := range g.conns {
		conns = append(conns, c)
	}
	g.mu.Unlock()
	for _, c := range conns {
		c.send(context.Background(), map[string]any{kJSONRPC: jsonRPCVersion, "id": id, kMethod: method, kParams: params})
	}
}

// DeliverCron delivers a cron result into a session as Hermes does for a
// local chat: session.info lists the input under an input_id that names the
// cron producer, message.start names its admission, and message.complete
// has no text, because Hermes writes the result as a note.
func (g *Gateway) DeliverCron(sessionID, text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[sessionID]
	admission := "cron-" + sessionID
	s.Revision++
	g.publishLocked(s, gateway.InfoEvent, map[string]any{kPending: []gateway.PendingAdmission{
		{AdmissionID: admission, Status: "queued", Text: text, InputID: `["cron","agent:main:local:dm:` + sessionID + `","00"]`}},
		kRevision: s.Revision, kExecGen: s.Generation})
	g.publishAdmissionLocked(s, "message.start", admission, map[string]any{})
	g.publishAdmissionLocked(s, "message.complete", admission, map[string]any{kText: "", kStatus: kComplete})
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
		c.send(context.Background(), map[string]any{kJSONRPC: jsonRPCVersion, kMethod: kEvent, kParams: map[string]any{
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
	if s.turnEnded {
		s.turnEvents, s.turnEnded = nil, false
	}
	s.turnEvents = append(s.turnEvents, ev)
	s.turnEnded = typ == kMessageComplete
	var p struct {
		Text *string `json:"text"`
	}
	_ = json.Unmarshal(raw, &p)
	switch {
	case typ == "message.delta" && p.Text != nil:
		s.reply += *p.Text
	case typ == "message.delta" || typ == "message.interim" || typ == "message.start" || typ == "message.complete" || typ == "tool.start":
		s.reply = ""
	}
	if g.ReplayMax > 0 && len(s.ring) > g.ReplayMax {
		drop := len(s.ring) - g.ReplayMax
		s.evicted = s.ring[drop-1].Seq
		s.ring = append([]gateway.Event(nil), s.ring[drop:]...)
	}
	for c := range s.subs {
		c.send(context.Background(), map[string]any{kJSONRPC: jsonRPCVersion, kMethod: kEvent, kParams: ev})
	}
}

// SetRunning marks a session's turn running or not, as a turn another
// client started.
func (g *Gateway) SetRunning(sessionID string, running bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessions[sessionID].Running = running
}

// FinishReply ends the running turn as Hermes does: the reply is stored in
// the transcript, then message.complete carries its whole text.
func (g *Gateway) FinishReply(sessionID, text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[sessionID]
	s.Running = false
	s.Messages = append(s.Messages, map[string]any{kRole: kAssistant, kText: text})
	g.publishLocked(s, "message.complete", map[string]any{kText: text, kStatus: kComplete})
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
	http.SetCookie(w, &http.Cookie{Name: "hermes_session_at", Value: v, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
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
	c.send(r.Context(), map[string]any{kJSONRPC: jsonRPCVersion, kMethod: kEvent, kParams: map[string]any{
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
		stall := g.stalls[req.Method]
		delete(g.stalls, req.Method)
		g.mu.Unlock()
		if stall != nil {
			select {
			case <-stall:
			case <-r.Context().Done():
				return
			}
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
		result, rerr, after := g.dispatchOrRefuse(c, req.Method, req.Params)
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
				// Down before the client sees the connection close, or a
				// retry can reach the gateway that has not stopped yet.
				g.Stop()
			}
			_ = ws.CloseNow()
			return
		}
		if errFrame != nil {
			c.send(r.Context(), map[string]any{kJSONRPC: jsonRPCVersion, "id": req.ID, "error": errFrame})
		} else {
			c.send(r.Context(), map[string]any{kJSONRPC: jsonRPCVersion, "id": req.ID, kResult: result})
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
	reason, detail, _ := strings.Cut(reason, "|")
	data := map[string]any{"reason": reason}
	if detail != "" {
		data["detail"] = detail
	}
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
	// A connection that resumes a session it is subscribed to again gets
	// the subscription it has, as Hermes' session authority answers.
	sub := s.subs[c]
	if sub == "" {
		sub = randHex()
	}
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
	if s.Running && s.turn != "" {
		pending = append([]gateway.PendingAdmission{{AdmissionID: s.turn, Status: statusStart, InputID: s.turnInput}}, pending...)
	}
	out := map[string]any{
		kSessionID: s.ID, "stored_session_id": s.ID, "messages": msgs, "message_count": len(msgs),
		"running": s.Running, "authority_epoch": 1, kReplayEpoch: s.epoch, kLastSequence: s.seq,
		"subscription_id": sub, kRevision: s.Revision, kExecGen: s.Generation,
		kPending: pending, "prompts": prompts,
		"info": g.infoLocked(s, true),
	}
	if g.InflightReply && s.Running && s.reply != "" {
		out["inflight_reply"] = map[string]any{kText: s.reply, kInflightSeq: s.seq}
	}
	if g.InflightTurn && s.Running {
		evs := []gateway.Event{}
		if !s.turnEnded {
			evs = append(evs, s.turnEvents...)
		}
		out["inflight_turn"] = map[string]any{kInflightEvents: evs, kInflightSeq: s.seq}
	}
	return out
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
	if s.Tools != nil {
		info["tools"] = s.Tools
	}
	if full {
		info[kProvider], info["context_length"] = s.Provider, 131072
		info["usage"] = map[string]any{kModel: s.Model, "input": 0, "output": 0, kReasoning: 0, "prompt": 0,
			"completion": 0, kTotal: 0, "calls": 0}
	}
	return info
}

// dispatchOrRefuse is dispatch after the refusals RefuseWhileDraining set.
func (g *Gateway) dispatchOrRefuse(c *conn, method string, p map[string]any) (any, string, func()) {
	if g.drainingCalls[method] > 0 {
		g.drainingCalls[method]--
		return nil, gateway.ReasonRuntimeDraining, nil
	}
	if reason := g.refusedCalls[method]; reason != "" {
		return nil, reason, nil
	}
	return g.dispatch(c, method, p)
}

func (g *Gateway) dispatch(c *conn, method string, p map[string]any) (any, string, func()) {
	var h func(*conn, map[string]any) (any, string, func())
	switch method {
	case gateway.MethodSessionList:
		h = g.hSessionList
	case methodRuntimeDescribe:
		h = g.hRuntimeDescribe
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
	case gateway.MethodConfigGet:
		h = g.hConfigGet
	case gateway.MethodPromptSubmit:
		h = g.hPromptSubmit
	case gateway.MethodInterrupt:
		h = g.hInterrupt
	case gateway.MethodApproval:
		h = g.hApproval
	case gateway.MethodClarify:
		h = g.hClarify
	case gateway.MethodPromptCancel:
		h = g.hPromptCancel
	case gateway.MethodResolveLost:
		h = g.hResolveLost
	case gateway.MethodMutate:
		h = g.hMutate
	case gateway.MethodModelOptions:
		h = g.hModelOptions
	case gateway.MethodAttachBytes:
		h = g.hAttachBytes
	case gateway.MethodFileAttach:
		h = g.hFileAttach
	case gateway.MethodSubagentList:
		h = g.hSubagentList
	case gateway.MethodSubagentTail:
		h = g.hSubagentTail
	case gateway.MethodSubagentSteer:
		h = g.hSubagentSteer
	case gateway.MethodSubagentInterrupt:
		h = g.hSubagentInterrupt
	case gateway.MethodSubagentRoster:
		h = g.hRoster
	case gateway.MethodProcessList:
		h = g.hProcessList
	case gateway.MethodProcessKill:
		h = g.hProcessKill
	case gateway.MethodDelegationStatus:
		h = g.hDelegationStatus
	case gateway.MethodDelegationPause:
		h = g.hDelegationPause
	case gateway.MethodBackgroundTool:
		h = g.hBackgroundTool
	default:
		if h = g.composeHandler(method); h == nil {
			return nil, "unknown method: " + method, nil
		}
	}
	return h(c, p)
}

func (g *Gateway) hSessionList(_ *conn, p map[string]any) (any, string, func()) {
	audience, scoped := p["audience"].(string)
	keep, judged := g.audiences[audience]
	switch {
	case scoped && !judged:
		return nil, errInvalidParams, nil
	case scoped && !onlyKeys(p, "limit", "title", "include_hidden", kScope, "audience", "picker", "workspace"):
		return nil, errInvalidParams, nil
	case !scoped && !onlyKeys(p, "limit", "title", "include_hidden", kScope, "picker"):
		return nil, errInvalidParams, nil
	}
	picker, _ := p["picker"].(bool)
	scope := str(p, kScope)
	if scope == "" {
		scope = "live"
	}
	hidden, _ := p["include_hidden"].(bool)
	rows := []map[string]any{}
	for _, s := range g.sessions {
		if scope == "live" && !s.Live || s.Archived && (scope == "stored" || !hidden) || scoped && !slices.Contains(keep, s.ID) {
			continue
		}
		source := s.Source
		if source == "" {
			source = gateway.SourceACP
		}
		row := map[string]any{kSessionID: s.ID, "id": s.ID, kTitle: s.Title, "source": source,
			"started_at": s.StartedAt, "message_count": len(s.Messages), "running": s.Running, kModel: s.Model}
		if picker {
			row["last_active_at"], row["pinned"] = nil, false
			if s.LastActiveAt > 0 {
				row["last_active_at"] = s.LastActiveAt
			}
		}
		rows = append(rows, row)
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
	snap := g.snapshotLocked(c, s)
	if g.ResendOnResume == 0 || !s.Running {
		return snap, "", nil
	}
	again := append([]gateway.Event(nil), s.ring[max(0, len(s.ring)-g.ResendOnResume):]...)
	return snap, "", func() {
		for _, ev := range again {
			c.send(context.Background(), map[string]any{kJSONRPC: jsonRPCVersion, kMethod: kEvent, kParams: ev})
		}
	}
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
	if str(p, kReplayEpoch) != s.epoch || last > s.seq || last < s.evicted {
		return map[string]any{"events": []any{}, kLastSequence: s.seq, kReplayEpoch: s.epoch,
			"snapshot_required": true, kTruncated: true}, "", nil
	}
	evs := []gateway.Event{}
	for _, ev := range s.ring {
		if ev.Seq > last {
			evs = append(evs, ev)
		}
	}
	return map[string]any{"events": evs, kLastSequence: s.seq, kReplayEpoch: s.epoch,
		"snapshot_required": false, kTruncated: false, "count": len(evs)}, "", nil
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

func (g *Gateway) hConfigGet(c *conn, p map[string]any) (any, string, func()) {
	s := g.sessions[str(p, kSessionID)]
	if s == nil || s.subs[c] == "" || !onlyKeys(p, kSessionID, kKey) || str(p, kKey) != gateway.KeyYolo {
		return nil, errInvalidParams, nil
	}
	v := "0"
	if s.Yolo {
		v = "1"
	}
	return map[string]any{kKey: gateway.KeyYolo, kValue: v, kScope: kSession}, "", nil
}

func (g *Gateway) hPromptSubmit(c *conn, p map[string]any) (any, string, func()) {
	s := g.sessions[str(p, kSessionID)]
	if s == nil || s.subs[c] == "" {
		return nil, errDenied, nil
	}
	if !onlyKeys(p, kSessionID, kText, kSubmission, "attachments") || str(p, kSubmission) == "" {
		return nil, errInvalidParams, nil
	}
	var atts []gateway.Attachment
	if raw, ok := p["attachments"]; ok {
		b, err := json.Marshal(raw)
		if err != nil || json.Unmarshal(b, &atts) != nil || len(atts) == 0 || len(atts) > 10 {
			return nil, errInvalidParams, nil
		}
		for _, a := range atts {
			if !strings.HasPrefix(a.Path, StagedImages) {
				return nil, errInvalidParams, nil
			}
		}
	}
	if r := g.refuse[s.ID]; r != "" {
		return nil, r, nil
	}
	key := s.ID + "\x00" + str(p, kSubmission)
	if old, ok := g.submissions[key]; ok {
		if old.text != str(p, kText) {
			return nil, "admission_conflict", nil
		}
		return old.receipt, "", nil
	}
	if g.QueueWhileRunning && s.Running {
		queued := g.queueLocked(s, str(p, kSubmission), str(p, kText))
		g.submissions[key] = submission{text: str(p, kText), receipt: queued}
		return queued, "", nil
	}
	s.Generation++
	s.Running = true
	s.Attachments = append(s.Attachments, atts)
	s.Messages = append(s.Messages, map[string]any{kRole: roleUser, kText: str(p, kText), kSubmission: str(p, kSubmission)})
	admission := randHex()
	started := receipt(s.ID, admission, 1, statusStart, s.Generation)
	g.submissions[key] = submission{text: str(p, kText), receipt: started}
	if g.HoldTurns {
		return started, "", nil
	}
	text := str(p, kText)
	return started, "", func() { g.turn(s.ID, admission, str(p, kSubmission), text) }
}

func (g *Gateway) hInterrupt(_ *conn, p map[string]any) (any, string, func()) {
	s := g.sessions[str(p, kSessionID)]
	gen, ok := num(p, kExecGen)
	if s == nil || !ok || gen != s.Generation {
		return nil, "stale_generation", nil
	}
	s.Running = false
	g.publishLocked(s, "message.complete", map[string]any{kText: "", kStatus: kInterrupted})
	g.settleTransfersLocked(s, "cancelled")
	queued := s.Pending
	s.Pending = nil
	return map[string]any{"execution_state": "idle", kExecGen: s.Generation}, "", func() { g.runQueued(s.ID, queued) }
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
	g.resolveApproval(s, pr, str(p, "choice"))
	return map[string]any{kStatus: "resolved"}, "", nil
}

// deniedResult is the result of a terminal call whose approval was denied
// on the gateway (tools/approval.py gateway_refused, tools/terminal_tool.py
// _error_json).
const deniedResult = `{"output": "", "exit_code": -1, "error": "BLOCKED: Command denied by user. The user has NOT consented ` +
	`to this action.", "status": "blocked"}`

// resolveApproval settles an approval with choice and ends the terminal
// call that asked for it as Hermes does. g.mu is held.
func (g *Gateway) resolveApproval(s *Session, pr gateway.Prompt, choice string) {
	delete(s.prompts, pr.PromptID)
	g.publishLocked(s, "approval.settled", map[string]any{"prompt_id": pr.PromptID, kExecGen: pr.ExecutionGeneration})
	result := `{"output": "approved: ` + choice + `", "exit_code": 0}`
	if choice == gateway.ChoiceDeny {
		result = deniedResult
	}
	g.publishLocked(s, "tool.complete", map[string]any{kToolID: "t-approve", kName: kTerminal, kResult: result})
	s.Running = false
	g.publishLocked(s, "message.complete", map[string]any{kText: "approved: " + choice, kStatus: kComplete})
}

// AnswerElsewhere answers a pending approval as a client that talks to the
// gateway directly would, so the hub sees only the settle and how the
// tool call ends.
func (g *Gateway) AnswerElsewhere(sessionID, promptID, choice string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[sessionID]
	if pr, ok := s.prompts[promptID]; ok {
		g.resolveApproval(s, pr, choice)
	}
}

func (g *Gateway) hClarify(_ *conn, p map[string]any) (any, string, func()) {
	if fmt.Sprint(keys(p)) != "[answer execution_generation prompt_id session_id]" {
		return nil, errInvalidParams, nil
	}
	s := g.sessions[str(p, kSessionID)]
	pr, ok := s.prompts[str(p, "prompt_id")]
	if !ok || pr.Kind != "clarify" {
		return map[string]any{kStatus: "already_resolved"}, "", nil
	}
	delete(s.prompts, pr.PromptID)
	g.publishLocked(s, "clarify.settled", map[string]any{"prompt_id": pr.PromptID, kExecGen: pr.ExecutionGeneration})
	s.Running = false
	g.publishLocked(s, "message.complete", map[string]any{kText: "answered: " + str(p, "answer"), kStatus: kComplete})
	return map[string]any{kStatus: "resolved"}, "", nil
}

// clarify raises a question: "ask me" asks with choices, "pick many" with
// several picks, "ask free" with none.
func (g *Gateway) clarify(s *Session, text string) bool {
	q := "Which one?"
	pr := gateway.Prompt{Kind: "clarify", PromptID: "cl-1", ExecutionGeneration: s.Generation, Question: &q}
	switch text {
	case "ask me":
		pr.Choices = []string{"red", "blue"}
	case "pick many":
		pr.Choices, pr.MultiSelect = []string{"red", "blue", "green"}, true
	case "ask free":
		pr.Choices = []string{}
	default:
		return false
	}
	s.prompts[pr.PromptID] = pr
	g.publishLocked(s, "clarify.request", pr)
	return true
}

func (g *Gateway) turn(id, admission, submission, text string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[id]
	g.startLocked(s, admission, submission, text)
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
	if g.clarify(s, text) {
		return
	}
	g.publishLocked(s, "tool.start", map[string]any{kToolID: "t1", kName: toolReadFile, kArgs: map[string]any{kPath: "README.md"}})
	g.publishLocked(s, "tool.progress", map[string]any{kToolID: "t1", kName: toolReadFile, "progress": 1, kTotal: 2})
	switch text {
	case "edit":
		g.publishLocked(s, "tool.start", map[string]any{kToolID: "t-edit", kName: "patch", kArgs: map[string]any{kPath: "a.go"}})
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
	s.Messages = append(s.Messages, map[string]any{kRole: "assistant", kText: "echo: " + text})
	g.publishLocked(s, "message.complete", map[string]any{kText: "echo: " + text, kStatus: kComplete,
		"usage": map[string]any{kModel: s.Model, "input": 9000, "output": 120, kReasoning: 0, "prompt": 9000, "completion": 120,
			kTotal: 9120, "calls": 1, "context_used": 9000, "context_max": 131072, "context_percent": 7,
			"context_source": "provider_usage", "context_estimated": false, "avg_tps": 42.5}})
	g.settleTransfersLocked(s, "steered")
}

// startLocked starts a turn as Hermes does: session.info lists the input
// with its text and the client's submission ID, then message.start names
// the admission with an empty payload.
func (g *Gateway) startLocked(s *Session, admission, submission, text string) {
	s.Revision++
	s.turn, s.turnInput = admission, submission
	g.publishLocked(s, gateway.InfoEvent, map[string]any{kPending: []gateway.PendingAdmission{
		{AdmissionID: admission, Status: "started", Text: text, InputID: submission}}, kRevision: s.Revision, kExecGen: s.Generation})
	g.publishAdmissionLocked(s, "message.start", admission, map[string]any{})
}

// StartElsewhere starts a turn as another client of the gateway does,
// such as the terminal UI, and leaves it running: the test publishes the
// rest. With offline set, no connection gets its events, as when the hub
// was not subscribed; with stored unset the transcript does not show the
// prompt yet.
func (g *Gateway) StartElsewhere(sessionID, submission, text string, offline, stored bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[sessionID]
	subs := s.subs
	if offline {
		s.subs = map[*conn]string{}
	}
	s.Generation++
	s.Running = true
	if stored {
		s.Messages = append(s.Messages, map[string]any{kRole: "user", kText: text, kSubmission: submission})
	}
	g.startLocked(s, randHex(), submission, text)
	s.subs = subs
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
		return map[string]any{"admission_id": id, kStatus: kTerminal, kOutcome: kInterrupted}, "", nil
	}
	return nil, gateway.ReasonNotFound, nil
}

// Where the fake says it staged attachments.
const (
	StagedImages = "/opt/data/cache/images/"
	StagedFiles  = "/opt/data/attachments/"
)

// hAttachBytes stages an image as gateway/session_images.py does.
func (g *Gateway) hAttachBytes(c *conn, p map[string]any) (any, string, func()) {
	s := g.sessions[str(p, kSessionID)]
	if s == nil || s.subs[c] == "" {
		return nil, errDenied, nil
	}
	data, err := base64.StdEncoding.DecodeString(str(p, "content_base64"))
	if !onlyKeys(p, kSessionID, "content_base64", "filename") || err != nil || len(data) == 0 || len(data) > 25<<20 {
		return nil, errInvalidParams, nil
	}
	mime := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}[path.Ext(str(p, "filename"))]
	if mime == "" {
		return nil, errInvalidParams, nil
	}
	name := "upload_" + randHex()[:8] + path.Ext(str(p, "filename"))
	return map[string]any{"attached": true, kPath: StagedImages + name, "mime": mime, kName: name, "bytes": len(data)}, "", nil
}

// hFileAttach stages a data URL as gateway/session_ancillary.py does.
func (g *Gateway) hFileAttach(c *conn, p map[string]any) (any, string, func()) {
	s := g.sessions[str(p, kSessionID)]
	if s == nil || s.subs[c] == "" {
		return nil, errDenied, nil
	}
	_, b64, ok := strings.Cut(str(p, "data_url"), ";base64,")
	if _, err := base64.StdEncoding.DecodeString(b64); !onlyKeys(p, kSessionID, "data_url", kName) || !ok || err != nil {
		return nil, errInvalidParams, nil
	}
	name := path.Base(str(p, kName))
	return map[string]any{"attached": true, kName: name, kPath: StagedFiles + name, "ref_path": StagedFiles + name,
		"ref_text": "@file:" + StagedFiles + name, "uploaded": true}, "", nil
}
