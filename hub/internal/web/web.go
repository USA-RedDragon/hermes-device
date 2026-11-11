// Package web serves the hub's mobile web UI and its JSON and
// server-sent-events API, behind OIDC login.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/google/uuid"
)

//go:embed static
var staticFiles embed.FS

// GroupStore keeps each user's groups from their latest login, for
// authorization outside a browser session (the Telegram bot).
type GroupStore interface {
	SetGroups(ctx context.Context, user string, groups []string) error
}

// StatusReporter is the optional part of a hub.Service that reports
// backend connection errors.
type StatusReporter interface {
	BackendStatus(ctx context.Context, user string) (map[string]string, error)
}

// Options are the dependencies of a Server.
type Options struct {
	Service hub.Service
	Links   hub.Links
	Tokens  hub.TokenStore
	Groups  GroupStore
	Logger  *slog.Logger
	// HTTPClient is used for OIDC requests. Default http.DefaultClient.
	HTTPClient *http.Client
	// KeepAlive is the interval of SSE comments that keep idle proxies
	// from closing a stream. Default 15s.
	KeepAlive time.Duration
	// Devices turns on the pairing page and the device list. Nil when
	// device.enabled is off.
	Devices hub.DeviceStore
	// DeviceAPI serves /api/device/, the API of the hermes-device client.
	DeviceAPI http.Handler
	// OnRevoke is called with the id of each device a user revokes, to
	// close its connections on this replica.
	OnRevoke func(deviceID string)
	// BrowserAPI serves /api/browser/, the sign-in API of the Hermes
	// Firefox extension.
	BrowserAPI http.Handler
	// OnBrowserRevoke is called with the device and the id of each browser
	// sign-in a user removes, to end it on this replica's legs at once.
	OnBrowserRevoke func(deviceID, sessionID string)
	// Sandboxes lists the service devices a user's device list shows.
	Sandboxes func(ctx context.Context, user string) []hub.Sandbox
	// Computers lets users start and move chats on their computers. Nil
	// when device.enabled is off.
	Computers hub.Computers
	// Memory serves the Memory page. Nil: the hub has no Memory page.
	Memory hub.Memory
}

// Server is the web front end.
type Server struct {
	svc          hub.Service
	links        hub.Links
	tokens       hub.TokenStore
	groups       GroupStore
	log          *slog.Logger
	oidc         *oidcClient
	sessions     *sealer
	logins       *sealer
	ttl          time.Duration
	origin       string
	secure       bool
	displayClaim string
	users        map[string]string
	groupsClaim  string
	keepAlive    time.Duration
	linkPage     *template.Template
	pairPage     *template.Template
	defaults     map[string]string
	devices      hub.DeviceStore
	deviceAPI    http.Handler
	onRevoke     func(string)
	browserAPI   http.Handler
	onBrowserOut func(string, string)
	consentPage  *template.Template
	sandboxes    func(context.Context, string) []hub.Sandbox
	computers    hub.Computers
	memory       hub.Memory
	// deviceBackendIDs are the backends with devices on.
	deviceBackendIDs map[string]bool
	backendNames     map[string]string
	instance         string
	ready            atomic.Bool
	draining         chan struct{}
	drainOnce        sync.Once
}

// New returns a Server.
func New(cfg *config.Config, opts Options) (*Server, error) {
	u, err := url.Parse(cfg.HTTP.PublicURL)
	if err != nil {
		return nil, err
	}
	sessions, err := newSealer(cfg.Session.Secret, "session")
	if err != nil {
		return nil, err
	}
	logins, err := newSealer(cfg.Session.Secret, "login")
	if err != nil {
		return nil, err
	}
	defaults, err := cfg.UI.DefaultBackendMap()
	if err != nil {
		return nil, err
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.KeepAlive == 0 {
		opts.KeepAlive = 15 * time.Second
	}
	deviceBackends, names := map[string]bool{}, map[string]string{}
	for _, b := range cfg.Backends {
		names[b.ID] = b.Name
		if b.Devices {
			deviceBackends[b.ID] = true
		}
	}
	return &Server{
		devices: opts.Devices, deviceAPI: opts.DeviceAPI, onRevoke: opts.OnRevoke,
		browserAPI: opts.BrowserAPI, onBrowserOut: opts.OnBrowserRevoke, consentPage: newConsentPage(), sandboxes: opts.Sandboxes, computers: opts.Computers, memory: opts.Memory,
		deviceBackendIDs: deviceBackends, backendNames: names, pairPage: newPairPage(),
		svc: opts.Service, links: opts.Links, tokens: opts.Tokens, groups: opts.Groups, log: opts.Logger,
		oidc: &oidcClient{issuer: cfg.OIDC.IssuerURL, clientID: cfg.OIDC.ClientID, clientSecret: cfg.OIDC.ClientSecret,
			redirect: cfg.HTTP.PublicURL + "/auth/callback", scopes: cfg.OIDC.Scopes, httpClient: opts.HTTPClient},
		sessions: sessions, logins: logins, ttl: cfg.Session.TTL,
		origin: u.Scheme + "://" + u.Host, secure: u.Scheme == "https",
		displayClaim: cfg.OIDC.DisplayClaim, users: cfg.UserMap(), groupsClaim: cfg.OIDC.GroupsClaim, keepAlive: opts.KeepAlive,
		defaults: defaults,
		linkPage: template.Must(template.New("link").Parse(linkHTML)),
		draining: make(chan struct{}),
		instance: uuid.NewString()[:8],
	}, nil
}

// SetReady sets what /readyz answers: 200 when ready, else 503, so load
// balancers stop sending requests before the hub shuts down.
func (s *Server) SetReady(ready bool) { s.ready.Store(ready) }

// Drain ends every event stream with a request to reconnect, which a
// browser does to another replica. Streams opened after it end at once.
func (s *Server) Drain() { s.drainOnce.Do(func() { close(s.draining) }) }

// Handler is the HTTP handler of the whole UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFiles, "static")
	files := http.FileServerFS(static)
	mux.Handle("GET /static/", http.StripPrefix("/static/", files))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, static, "index.html")
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/callback", s.handleCallback)
	mux.Handle("POST /auth/logout", s.api(s.handleLogout))
	mux.Handle("GET /api/me", s.api(s.handleMe))
	mux.Handle("GET /api/backends", s.api(s.handleBackends))
	mux.Handle("GET /api/sessions", s.api(s.handleSessions))
	mux.Handle("POST /api/sessions", s.api(s.handleNewSession))
	mux.Handle("GET /api/sessions/{backend}/{session}/events", s.api(s.handleEvents))
	mux.Handle("POST /api/sessions/{backend}/{session}/prompt", s.api(s.handlePrompt))
	mux.Handle("POST /api/sessions/{backend}/{session}/interrupt", s.api(s.handleInterrupt))
	mux.Handle("POST /api/sessions/{backend}/{session}/approvals/{approval}", s.api(s.handleApproval))
	mux.Handle("POST /api/sessions/{backend}/{session}/inputs/{input}/cancel", s.api(s.handleCancelInput))
	mux.Handle("POST /api/sessions/{backend}/{session}/inputs/{input}/dismiss", s.api(s.handleDismissLost))
	mux.Handle("POST /api/sessions/{backend}/{session}/rename", s.api(s.handleRename))
	mux.Handle("POST /api/sessions/{backend}/{session}/archive", s.api(s.handleArchive))
	mux.Handle("POST /api/sessions/{backend}/{session}/delete", s.api(s.handleDelete))
	mux.Handle("GET /api/sessions/{backend}/{session}/models", s.api(s.handleModels))
	mux.Handle("GET /api/sessions/{backend}/{session}/media", s.api(s.handleMedia))
	mux.Handle("GET /api/sessions/{backend}/{session}/page", s.api(s.handlePage))
	mux.Handle("GET /api/sessions/{backend}/{session}/page-info", s.api(s.handlePageInfo))
	mux.Handle("POST /api/sessions/{backend}/{session}/model", s.api(s.handleSetModel))
	mux.Handle("POST /api/sessions/{backend}/{session}/reasoning", s.api(s.handleSetReasoning))
	mux.Handle("GET /api/sessions/{backend}/{session}/place", s.api(s.handlePlace))
	mux.Handle("POST /api/sessions/{backend}/{session}/move", s.api(s.handleMove))
	mux.Handle("POST /api/sessions/{backend}/{session}/release", s.api(s.handleRelease))
	mux.Handle("POST /api/places", s.api(s.handlePlaces))
	mux.Handle("GET /api/computers", s.api(s.handleComputers))
	mux.Handle("GET /api/computers/{device}/folders", s.api(s.handleFolders))
	s.memoryRoutes(mux)
	mux.HandleFunc("GET /link/telegram", s.handleLinkPage)
	mux.HandleFunc("POST /link/telegram", s.handleLink)
	mux.HandleFunc("GET /device/pair", s.handlePairPage)
	mux.HandleFunc("POST /device/pair", s.handlePair)
	mux.Handle("GET /api/devices", s.api(s.handleDevices))
	mux.Handle("POST /api/devices/{device}/revoke", s.api(s.handleRevokeDevice))
	mux.Handle("POST /api/devices/{device}/dismiss-address", s.api(s.handleDismissAddress))
	if s.deviceAPI != nil {
		mux.Handle("/api/device/", s.deviceAPI)
	}
	mux.HandleFunc("GET /browser/authorize", s.handleBrowserAuthorizePage)
	mux.HandleFunc("POST /browser/authorize", s.handleBrowserAuthorize)
	mux.Handle("POST /api/devices/{device}/browsers/{browser}/revoke", s.api(s.handleRevokeBrowser))
	if s.browserAPI != nil {
		mux.Handle("/api/browser/", s.browserAPI)
	}
	return headers(mux)
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		// no-referrer would make browsers send "Origin: null" on form posts, which the origin check refuses
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// originOK rejects a request whose Origin (or, without one, Sec-Fetch-Site)
// shows it came from another site.
func (s *Server) originOK(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return o == s.origin
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin" || site == "none"
}

type userKey struct{}

func userFrom(r *http.Request) string {
	u, _ := r.Context().Value(userKey{}).(string)
	return u
}

// api requires a session, and for anything but GET an X-CSRF-Token header
// that matches it and a same-origin request.
func (s *Server) api(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.session(r)
		if !ok {
			errorJSON(w, http.StatusUnauthorized, "login required")
			return
		}
		if !s.originOK(r) {
			errorJSON(w, http.StatusForbidden, "cross-origin request")
			return
		}
		if r.Method != http.MethodGet {
			tok := r.Header.Get("X-CSRF-Token")
			if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(sess.CSRF)) != 1 {
				errorJSON(w, http.StatusForbidden, "bad CSRF token")
				return
			}
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey{}, sess.User)))
	})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	if computerError(w, err) {
		return
	}
	switch {
	case errors.Is(err, hub.ErrForbidden):
		errorJSON(w, http.StatusForbidden, "not allowed")
	case errors.Is(err, hub.ErrUnknownBackend):
		errorJSON(w, http.StatusNotFound, "unknown backend")
	case errors.Is(err, hub.ErrUnknownApproval):
		errorJSON(w, http.StatusNotFound, "no such pending approval")
	case errors.Is(err, hub.ErrUnknownInput):
		errorJSON(w, http.StatusNotFound, "that message is no longer waiting")
	case errors.Is(err, hub.ErrUnknownSession):
		errorJSON(w, http.StatusNotFound, "that chat no longer exists")
	case errors.Is(err, hub.ErrBusy):
		errorJSON(w, http.StatusConflict, "Hermes is still working on this chat. Stop it or wait until it is done, then try again")
	case errors.Is(err, hub.ErrHoldFull):
		errorJSON(w, http.StatusTooManyRequests, "Too many messages are waiting for Hermes to come back. Send this one again later")
	case errors.Is(err, hub.ErrShuttingDown):
		errorJSON(w, http.StatusServiceUnavailable, "The hub is restarting. Send the message again in a moment")
	case errors.Is(err, hub.ErrTitleInUse):
		errorJSON(w, http.StatusConflict, "Hermes did not accept that name. Another chat may already use it")
	case errors.Is(err, hub.ErrAttachment):
		errorJSON(w, http.StatusBadRequest, hub.AttachmentReason(err))
	case errors.Is(err, hub.ErrBadValue):
		msg := strings.TrimPrefix(err.Error(), hub.ErrBadValue.Error()+": ")
		if !strings.Contains(msg, " ") {
			msg = "Hermes did not accept that"
		}
		errorJSON(w, http.StatusBadRequest, msg)
	default:
		s.log.Warn("request failed", "error", err.Error())
		msg := err.Error()
		if i := strings.LastIndex(msg, ": "); i >= 0 && len(msg)-i < 120 {
			msg = msg[i+2:]
		}
		errorJSON(w, http.StatusBadGateway, msg)
	}
}
