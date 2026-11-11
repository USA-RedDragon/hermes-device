// Package splice is the run daemon's loopback gateway splice: the local
// Hermes TUI dials it, and it forwards to the hub's gateway leg with the
// rewrites and denials of docs/protocol.md section 5.
package splice

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

// Paths the splice serves.
const (
	PathTUI           = "/api/ws"
	PathControlTokens = "/control/tokens"
	PathControlLookup = "/control/lookup"
	// PathControlBrowser answers GET with the Firefox extension's state, a
	// BrowserStatus.
	PathControlBrowser = "/control/browser"
	// PathControlBrowserSignIn asks the connected extension to open the
	// hub's sign-in window.
	PathControlBrowserSignIn = "/control/browser/sign-in"
)

const (
	dialTimeout   = 30 * time.Second
	deleteTimeout = 30 * time.Second
	maxFrame      = 64 << 20
)

// Options configure a Server.
type Options struct {
	// Hub dials the gateway leg and binds sessions.
	Hub *hubapi.Client
	// PingInterval is the WebSocket ping interval on both hops. Zero means
	// protocol.PingInterval.
	PingInterval time.Duration
	// PingTimeout is the pong deadline on both hops. Zero means
	// protocol.PingTimeout.
	PingTimeout time.Duration
	// Logger gets the splice's logs. Nil means slog.Default().
	Logger *slog.Logger
	// Browser is the daemon's Firefox control, or nil when browser.enabled
	// is off.
	Browser Browser
}

// Browser is what the control endpoints need of the daemon's Firefox
// control.
type Browser interface {
	Status() BrowserStatus
	// RequestSignIn asks the extension to open the hub's sign-in window
	// and reports whether an extension was connected.
	RequestSignIn() bool
}

// BrowserStatus is the answer to GET /control/browser.
type BrowserStatus struct {
	// Enabled is false when browser.enabled is off; the other fields are
	// then empty.
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	Version   string `json:"version,omitempty"`
	Browser   string `json:"browser,omitempty"`
	// Auth is one of the broker's Auth* sign-in states.
	Auth   string `json:"auth,omitempty"`
	User   string `json:"user,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Server serves /api/ws for the TUI and /control/tokens for chat.
type Server struct {
	opts          Options
	log           *slog.Logger
	controlSecret string

	mu     sync.Mutex
	tokens map[string]*tokenEntry
	// closed is set once Serve ended; new sockets are refused.
	closed bool
	// active counts TUI sockets being served.
	active sync.WaitGroup

	phMu        sync.Mutex
	placeholder string

	bindMu sync.Mutex
	// binds caches successful create binds by request_id.
	binds map[string]createBind
	// dirs is the device directory of every session this daemon bound.
	dirs map[string]string
}

// tokenEntry is one per-run secret: one hermes-device chat.
type tokenEntry struct {
	// dir is the device directory chat runs in.
	dir   string
	conns map[*conn]struct{}

	mu sync.Mutex
	// seen holds the sessions this chat bound, so a resume after a
	// reconnect keeps a binding a workspace move changed.
	seen map[string]bool
}

func (t *tokenEntry) saw(sessionID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seen[sessionID]
}

func (t *tokenEntry) see(sessionID string) {
	t.mu.Lock()
	t.seen[sessionID] = true
	t.mu.Unlock()
}

type createBind struct {
	sessionID string
	dir       string
}

// New returns a server with a fresh control secret.
func New(opts Options) *Server {
	if opts.PingInterval <= 0 {
		opts.PingInterval = protocol.PingInterval
	}
	if opts.PingTimeout <= 0 {
		opts.PingTimeout = protocol.PingTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		opts: opts, log: log.With("component", "splice"), controlSecret: NewSecret(),
		tokens: map[string]*tokenEntry{}, binds: map[string]createBind{}, dirs: map[string]string{},
	}
}

// NewSecret returns 32 random bytes as base64url without padding.
func NewSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ControlSecret is the secret chat sends to /control/tokens.
func (s *Server) ControlSecret() string { return s.controlSecret }

// Serve serves on ln until ctx ends. It closes every TUI socket before it
// returns.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathControlTokens, s.handleCreateToken)
	mux.HandleFunc("DELETE "+PathControlTokens+"/{token}", s.handleDeleteToken)
	mux.HandleFunc("POST "+PathControlLookup, s.handleLookup)
	mux.HandleFunc("GET "+PathControlBrowser, s.handleBrowser)
	mux.HandleFunc("POST "+PathControlBrowserSignIn, s.handleBrowserSignIn)
	mux.HandleFunc(PathTUI, s.handleTUI)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { forbidden(w) })
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	var err error
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	s.mu.Lock()
	s.closed = true
	var all []*conn
	for _, te := range s.tokens {
		for c := range te.conns {
			all = append(all, c)
		}
	}
	s.tokens = map[string]*tokenEntry{}
	s.mu.Unlock()
	for _, c := range all {
		c.closeTUI(websocket.StatusGoingAway, "hermes-device run is stopping")
	}
	s.active.Wait()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func forbidden(w http.ResponseWriter) {
	http.Error(w, "forbidden", http.StatusForbidden)
}

func loopbackPeer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) controlAllowed(r *http.Request) bool {
	if !loopbackPeer(r) {
		return false
	}
	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(secret), []byte(s.controlSecret)) == 1
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	if !s.controlAllowed(r) {
		forbidden(w)
		return
	}
	var body struct {
		Cwd string `json:"cwd"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil ||
		!filepath.IsAbs(body.Cwd) {
		http.Error(w, `body must be {"cwd": "<absolute directory>"}`, http.StatusBadRequest)
		return
	}
	token := NewSecret()
	s.mu.Lock()
	closed := s.closed
	if !closed {
		s.tokens[token] = &tokenEntry{dir: filepath.Clean(body.Cwd), conns: map[*conn]struct{}{}, seen: map[string]bool{}}
	}
	s.mu.Unlock()
	if closed {
		http.Error(w, "stopping", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(map[string]string{"token": token}); err != nil {
		s.log.Debug("token answer not written", keyError, err)
	}
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	if !s.controlAllowed(r) {
		forbidden(w)
		return
	}
	var conns []*conn
	s.mu.Lock()
	if t, ok := s.matchToken(r.PathValue("token")); ok {
		for c := range s.tokens[t].conns {
			conns = append(conns, c)
		}
		delete(s.tokens, t)
	}
	s.mu.Unlock()
	for _, c := range conns {
		c.closeTUI(websocket.StatusNormalClosure, "token revoked")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) {
	if !s.controlAllowed(r) {
		forbidden(w)
		return
	}
	var body struct {
		Session *string `json:"session"`
		Latest  bool    `json:"latest"`
	}
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	if err != nil || (body.Session == nil) == !body.Latest || (body.Session != nil && *body.Session == "") {
		http.Error(w, `body must be {"session": "<id or title>"} or {"latest": true}`, http.StatusBadRequest)
		return
	}
	name := ""
	if body.Session != nil {
		name = *body.Session
	}
	sid, err := s.Lookup(r.Context(), name)
	var le *LookupError
	switch {
	case errors.Is(err, ErrNoSession):
		s.writeControlJSON(w, http.StatusNotFound, map[string]string{keyError: "not_found"})
	case errors.As(err, &le):
		s.log.Warn("session lookup failed", keyError, err)
		s.writeControlJSON(w, http.StatusBadGateway, map[string]string{keyError: le.HubError, "message": err.Error()})
	case err != nil:
		s.writeControlJSON(w, http.StatusBadGateway, map[string]string{keyError: hubUnreachable, "message": err.Error()})
	default:
		s.writeControlJSON(w, http.StatusOK, map[string]string{keySessionID: sid})
	}
}

func (s *Server) handleBrowser(w http.ResponseWriter, r *http.Request) {
	if !s.controlAllowed(r) {
		forbidden(w)
		return
	}
	var st BrowserStatus
	if s.opts.Browser != nil {
		st = s.opts.Browser.Status()
		st.Enabled = true
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(st); err != nil {
		s.log.Debug("browser status not written", keyError, err)
	}
}

func (s *Server) handleBrowserSignIn(w http.ResponseWriter, r *http.Request) {
	if !s.controlAllowed(r) {
		forbidden(w)
		return
	}
	if s.opts.Browser == nil || !s.opts.Browser.RequestSignIn() {
		http.Error(w, "the Hermes Firefox extension is not connected", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeControlJSON(w http.ResponseWriter, status int, v map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Debug("control answer not written", "error", err)
	}
}

// matchToken returns the stored token equal to t, comparing it with every
// token in constant time. The caller holds s.mu.
func (s *Server) matchToken(t string) (string, bool) {
	if t == "" {
		return "", false
	}
	found := ""
	for k := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(k), []byte(t)) == 1 {
			found = k
		}
	}
	return found, found != ""
}

func (s *Server) handleTUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != PathTUI || !loopbackPeer(r) || len(r.Header.Values("Origin")) > 0 {
		forbidden(w)
		return
	}
	s.mu.Lock()
	token, ok := s.matchToken(r.URL.Query().Get("token"))
	ok = ok && !s.closed
	s.mu.Unlock()
	if !ok {
		forbidden(w)
		return
	}
	ctx := r.Context()
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	hub, err := s.opts.Hub.DialLeg(dialCtx, hubapi.PathGatewayLeg)
	cancel()
	if err != nil {
		s.log.Warn("gateway leg dial failed", keyError, err)
		http.Error(w, "hub gateway leg unavailable", http.StatusBadGateway)
		return
	}
	hub.SetReadLimit(maxFrame)
	tui, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		_ = hub.CloseNow()
		return
	}
	tui.SetReadLimit(maxFrame)
	s.mu.Lock()
	te, live := s.tokens[token]
	live = live && !s.closed
	var c *conn
	if live {
		c = newConn(s, te, tui, hub)
		te.conns[c] = struct{}{}
		s.active.Add(1)
	}
	s.mu.Unlock()
	if !live {
		_ = hub.CloseNow()
		_ = tui.Close(websocket.StatusNormalClosure, "token revoked")
		return
	}
	defer s.active.Done()
	c.run(ctx)
	s.mu.Lock()
	delete(te.conns, c)
	s.mu.Unlock()
}

// placeholderDir returns the server placeholder directory from whoami,
// cached after the first success.
func (s *Server) placeholderDir(ctx context.Context) (string, error) {
	s.phMu.Lock()
	defer s.phMu.Unlock()
	if s.placeholder != "" {
		return s.placeholder, nil
	}
	who, err := s.opts.Hub.Whoami(ctx)
	if err != nil {
		return "", err
	}
	if who.Placeholder == "" {
		return "", &hubapi.Error{Status: http.StatusBadGateway, Code: "no_placeholder", Message: "whoami has no placeholder"}
	}
	s.placeholder = who.Placeholder
	return s.placeholder, nil
}

// forgetPlaceholder drops the cached placeholder, so the next create asks
// whoami again.
func (s *Server) forgetPlaceholder() {
	s.phMu.Lock()
	s.placeholder = ""
	s.phMu.Unlock()
}

func (s *Server) cachedCreate(requestID, sessionID, dir string) bool {
	if requestID == "" {
		return false
	}
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	b, ok := s.binds[requestID]
	return ok && b.sessionID == sessionID && b.dir == dir
}

func (s *Server) rememberCreate(requestID, sessionID, dir string) {
	if requestID == "" {
		return
	}
	s.bindMu.Lock()
	s.binds[requestID] = createBind{sessionID: sessionID, dir: dir}
	s.bindMu.Unlock()
}

func (s *Server) sessionDir(sessionID string) (string, bool) {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	d, ok := s.dirs[sessionID]
	return d, ok
}

// bind binds sessionID to dir and remembers the device directory the hub
// answered with.
func (s *Server) bind(ctx context.Context, sessionID, dir string) error {
	b, err := s.opts.Hub.Bind(ctx, sessionID, dir)
	if err != nil {
		return err
	}
	bound := b.Dir
	if bound == "" {
		bound = dir
	}
	s.bindMu.Lock()
	s.dirs[sessionID] = bound
	s.bindMu.Unlock()
	return nil
}
