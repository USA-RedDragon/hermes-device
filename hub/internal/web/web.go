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
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/agentcfg"
	"github.com/USA-RedDragon/talaria/hub/internal/auth"
	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
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
	// DeviceAPI serves /api/device/, the API of the talaria client.
	DeviceAPI http.Handler
	// OnRevoke is called with the id of each device a user revokes, to
	// close its connections on this replica.
	OnRevoke func(deviceID string)
	// Sandboxes lists the service devices a user's device list shows.
	Sandboxes func(ctx context.Context, user string) []hub.Sandbox
	// Computers lets users start and move chats on their computers. Nil
	// when device.enabled is off.
	Computers hub.Computers
	// Memory serves the Memory page. Nil: the hub has no Memory page.
	Memory hub.Memory
	// Models connects model providers to Hermes from Settings > Models.
	// Nil: models are set up on the server only.
	Models hub.ModelSetup
	// Insights serves chat search and the Usage page. Nil: neither.
	Insights hub.Insights
	// Files serves the file viewer of chats on computers. Nil: none.
	Files hub.ChatFiles
	// Notices keeps notification settings, push subscriptions and
	// presence. Nil: no notification settings.
	Notices hub.NotifyStore
	// Notifier sends test notices and has the Web Push key.
	Notifier Notifier
	// Claude serves the Claude accounts of backends with the
	// Claude plugin. Nil: none.
	Claude hub.Claude
	// ClaudeCode serves the Usage page's Claude Code panel. Nil: no panel.
	ClaudeCode hub.ClaudeCode
	// Subagents serves the Subagents setting of chats. Nil: no picker.
	Subagents hub.SubagentRoutes
	// Chats keeps each user's drafts, pins and read state. Nil: none.
	Chats hub.ChatStore
	// Prompts keeps each user's saved prompts. Nil: none.
	Prompts hub.PromptLibrary
	// Partners keeps each user's chat pairs that work together. Nil: none.
	Partners hub.AgentPartners
	// AgentMessages looks up agent messages for the hop and sender shown
	// on their cards. Nil: the cards show neither.
	AgentMessages hub.AgentMessageReader
	// Version is the hub's release, shown in Settings.
	Version string
	// Scheduled serves the Scheduled page. Nil: no Scheduled page.
	Scheduled hub.Scheduled
	// Accounts keeps the hub's own accounts. Nil: only users[] log in.
	Accounts AccountStore
	// Agent drives the agent's own settings from Settings and the chat's
	// quick panel. Nil: none.
	Agent *agentcfg.Service
	// Telegram sets the bot token from Settings > Telegram. Nil: none.
	Telegram hub.TelegramAdmin
	// Updates says which parts are behind, for the admin banner. Nil: no
	// banner.
	Updates UpdateChecker
	// Snapshot copies the hub's database to a file for a backup. Nil: the
	// backup has no hub data.
	Snapshot func(ctx context.Context, path string) error
	// Scratch is where a backup's hub data is put together. Default the
	// system's temporary folder.
	Scratch string
	// Started is when the hub started, for Settings > Diagnostics.
	Started time.Time
	// Hasher hashes passwords. Default auth.DefaultParams, 4 at once.
	Hasher *auth.Hasher
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
	refusedPage  *template.Template
	proxyPage    *template.Template
	defaults     map[string]string
	devices      hub.DeviceStore
	deviceAPI    http.Handler
	onRevoke     func(string)
	sandboxes    func(context.Context, string) []hub.Sandbox
	computers    hub.Computers
	memory       hub.Memory
	models       hub.ModelSetup
	insights     hub.Insights
	files        hub.ChatFiles
	notices      hub.NotifyStore
	notifier     Notifier
	claude       hub.Claude
	scheduled    hub.Scheduled
	claudeCode   hub.ClaudeCode
	subagents    hub.SubagentRoutes
	chats        hub.ChatStore
	pruneMu      sync.Mutex
	pruned       map[string]time.Time
	prompts      hub.PromptLibrary
	partners     hub.AgentPartners
	// partnerOwners maps each single-user backend to its user.
	partnerOwners map[string]string
	// agentMessages gives the cards of agent messages their hop. Nil: none.
	agentMessages hub.AgentMessageReader
	authState
	// deviceBackendIDs are the backends with devices on.
	deviceBackendIDs map[string]bool
	backendNames     map[string]string
	instance         string
	agent            *agentcfg.Service
	agentAdmins      map[string][]string
	telegram         hub.TelegramAdmin
	updates          UpdateChecker
	snapshot         func(ctx context.Context, path string) error
	scratch          string
	hubKeys          string
	started          time.Time
	version          string
	install          string
	telegramSteps    string
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
	deviceBackends, names, owners := map[string]bool{}, map[string]string{}, map[string]string{}
	for _, b := range cfg.Backends {
		names[b.ID] = b.Name
		if len(b.Users) == 1 {
			owners[b.ID] = b.Users[0]
		}
		if b.Devices {
			deviceBackends[b.ID] = true
		}
	}
	st, err := newAuthState(cfg, opts)
	if err != nil {
		return nil, err
	}
	agentAdmins, err := cfg.Agent.AdminMap()
	if err != nil {
		return nil, err
	}
	return &Server{
		agent: opts.Agent, agentAdmins: agentAdmins, telegram: opts.Telegram, updates: opts.Updates,
		snapshot: opts.Snapshot, scratch: opts.Scratch, hubKeys: hubKeys(cfg), started: opts.Started,
		authState: st,
		devices:   opts.Devices, deviceAPI: opts.DeviceAPI, onRevoke: opts.OnRevoke, sandboxes: opts.Sandboxes, computers: opts.Computers, memory: opts.Memory, models: opts.Models,
		insights: opts.Insights, files: opts.Files,
		notices: opts.Notices, notifier: opts.Notifier, claude: opts.Claude, scheduled: opts.Scheduled,
		claudeCode: opts.ClaudeCode, subagents: opts.Subagents, chats: opts.Chats, prompts: opts.Prompts,
		partners: opts.Partners, agentMessages: opts.AgentMessages, partnerOwners: owners,
		deviceBackendIDs: deviceBackends, backendNames: names, pairPage: newPairPage(),
		svc: opts.Service, links: opts.Links, tokens: opts.Tokens, groups: opts.Groups, log: opts.Logger,
		oidc: &oidcClient{issuer: cfg.OIDC.IssuerURL, clientID: cfg.OIDC.ClientID, clientSecret: cfg.OIDC.ClientSecret,
			redirect: cfg.HTTP.PublicURL + "/auth/callback", scopes: cfg.OIDC.Scopes, httpClient: opts.HTTPClient},
		sessions: sessions, logins: logins, ttl: cfg.Session.TTL,
		origin: u.Scheme + "://" + u.Host, secure: u.Scheme == "https",
		displayClaim: cfg.OIDC.DisplayClaim, users: cfg.UserMap(), groupsClaim: cfg.OIDC.GroupsClaim, keepAlive: opts.KeepAlive,
		defaults:    defaults,
		linkPage:    template.Must(template.New("link").Parse(linkHTML)),
		refusedPage: template.Must(template.New("refused").Parse(refusedHTML)),
		proxyPage:   template.Must(template.New("proxy").Parse(proxyErrorHTML)),
		draining:    make(chan struct{}),
		instance:    uuid.NewString()[:8],
		version:     opts.Version, telegramSteps: telegramSteps(cfg), install: cfg.UI.Install,
	}, nil
}

// authState is how the server logs people in.
type authState struct {
	mode         string
	accounts     AccountStore
	accountCache *accountCache
	hasher       *auth.Hasher
	limiter      *auth.Limiter
	trusted      []netip.Prefix
	proxy        *auth.Proxy
	local        config.Local
	admins       map[string]bool
	// modelAdmins are models.admins, the model admins of each backend.
	// Empty: the hub's admins.
	modelAdmins  map[string][]string
	configNames  map[string]bool
	backendIDs   []string
	backendViews []backendView
	oidcAuto     bool
	oidcDomains  []string
	oidcGroups   []string
	emailClaim   string
	proxyAuto    bool
	logoutURL    string
	// loginInfo tells Settings how people get in, for the Users banner
	// and the Account page.
	loginInfo map[string]any
	issuer    string
}

func newAuthState(cfg *config.Config, opts Options) (authState, error) {
	a := cfg.Auth
	trusted, err := a.Proxies()
	if err != nil {
		return authState{}, err
	}
	s := authState{mode: a.Mode, accounts: opts.Accounts, accountCache: &accountCache{}, hasher: opts.Hasher, limiter: auth.NewLimiter(loginsPerWindow, loginWindow),
		trusted: trusted, local: a.Local, admins: map[string]bool{}, configNames: map[string]bool{},
		oidcAuto: cfg.OIDC.AutoCreate, oidcDomains: cfg.OIDC.AllowedDomains, oidcGroups: cfg.OIDC.AllowedGroups,
		emailClaim: cfg.OIDC.EmailClaim, proxyAuto: a.Proxy.AutoCreate}
	if s.mode == "" {
		s.mode = config.AuthOIDC
	}
	if s.hasher == nil {
		s.hasher = auth.NewHasher(auth.DefaultParams(), 4)
	}
	for _, name := range a.Admins {
		s.admins[name] = true
	}
	if s.modelAdmins, err = cfg.Models.AdminMap(); err != nil {
		return authState{}, err
	}
	for _, u := range cfg.Users {
		s.configNames[u.Name] = true
	}
	// A config written before auth.admins has no admin and no database
	// account to make one in, so its first user is the admin.
	if s.mode == config.AuthOIDC && len(s.admins) == 0 && len(cfg.Users) > 0 {
		s.admins[cfg.Users[0].Name] = true
	}
	for _, b := range cfg.Backends {
		s.backendIDs = append(s.backendIDs, b.ID)
		s.backendViews = append(s.backendViews, backendView{ID: b.ID, Name: b.Name, Everyone: b.Everyone,
			Users: nonNil(b.Users), Groups: nonNil(b.Groups)})
	}
	if s.mode == config.AuthProxy {
		if s.proxy, err = auth.NewProxy(a, opts.HTTPClient); err != nil {
			return authState{}, err
		}
		s.logoutURL = a.Proxy.LogoutURL
	}
	s.loginInfo = loginInfo(cfg, s.mode, trusted)
	s.issuer = hostOf(cfg.OIDC.IssuerURL)
	if s.mode == config.AuthLocal && s.accounts == nil {
		return authState{}, errLocalNeedsAccounts
	}
	return s, nil
}

var errLocalNeedsAccounts = errors.New("web: auth.mode local needs an account store")

// hubKeys is the keys part of a backup: the hub's own secrets, as YAML.
func hubKeys(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString("# The hub's keys. Keep this file private.\n")
	line := func(k, v string) {
		if v != "" {
			b.WriteString(k + ": " + strconv.Quote(v) + "\n")
		}
	}
	line("session_secret", cfg.Session.Secret)
	line("device_signing_key", cfg.Device.SigningKey)
	if cfg.Device.SigningKeyFile != "" {
		if data, err := os.ReadFile(cfg.Device.SigningKeyFile); err == nil {
			line("device_signing_key", strings.TrimSpace(string(data)))
		}
	}
	line("vapid_private_key", cfg.Notify.VAPIDPrivateKey)
	line("telegram_token", cfg.Telegram.Token)
	return b.String()
}

// telegramSteps is the bot's steps setting, or "" without a bot.
func telegramSteps(cfg *config.Config) string {
	if !cfg.Telegram.Enabled {
		return ""
	}
	if cfg.Telegram.Steps == "" {
		return "collapsed"
	}
	return cfg.Telegram.Steps
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
		if s.needsSetup(r.Context()) {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, static, "index.html")
	})
	mux.HandleFunc("GET /pagechat", s.handlePageChatView(static))
	mux.HandleFunc("GET /login", s.handleLoginPage(static))
	mux.HandleFunc("GET /setup", s.handleSetupPage(static))
	for _, f := range []struct{ path, file, mime string }{
		{"/manifest.webmanifest", "manifest.webmanifest", "application/manifest+json"},
		{"/sw.js", "sw.js", "text/javascript; charset=utf-8"},
	} {
		mux.HandleFunc("GET "+f.path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", f.mime)
			http.ServeFileFS(w, r, static, f.file)
		})
	}
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
	mux.Handle("POST /api/pagechats", s.api(s.handleNewPageChat))
	mux.Handle("GET /api/sessions/{backend}/{session}/events", s.api(s.handleEvents))
	mux.Handle("POST /api/sessions/{backend}/{session}/prompt", s.api(s.handlePrompt))
	mux.Handle("POST /api/sessions/{backend}/{session}/interrupt", s.api(s.handleInterrupt))
	mux.Handle("POST /api/sessions/{backend}/{session}/approvals/{approval}", s.api(s.handleApproval))
	mux.Handle("POST /api/sessions/{backend}/{session}/inputs/{input}/cancel", s.api(s.handleCancelInput))
	mux.Handle("POST /api/sessions/{backend}/{session}/inputs/{input}/dismiss", s.api(s.handleDismissLost))
	mux.Handle("POST /api/sessions/{backend}/{session}/inputs/{input}/now", s.api(s.handleStopAndSend))
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
	mux.Handle("GET /api/sessions/{backend}/{session}/activity", s.api(s.handleActivity))
	mux.Handle("GET /api/sessions/{backend}/{session}/subagents/{subagent}/tail", s.api(s.handleSubagentTail))
	mux.Handle("GET /api/sessions/{backend}/{session}/subagents/{subagent}/events", s.api(s.handleSubagentEvents))
	mux.Handle("POST /api/sessions/{backend}/{session}/subagents/{subagent}/steer", s.api(s.handleSteerSubagent))
	mux.Handle("POST /api/sessions/{backend}/{session}/subagents/{subagent}/stop", s.api(s.handleStopSubagent))
	mux.Handle("POST /api/sessions/{backend}/{session}/processes/{process}/stop", s.api(s.handleStopProcess))
	mux.Handle("POST /api/sessions/{backend}/{session}/tools/{tool}/background", s.api(s.handleBackgroundTool))
	mux.Handle("POST /api/sessions/{backend}/{session}/subagents-paused", s.api(s.handlePauseSubagents))
	mux.Handle("GET /api/sessions/{backend}/{session}/subagents", s.api(s.handleSubagents))
	mux.Handle("PUT /api/sessions/{backend}/{session}/subagents", s.api(s.handleSubagents))
	mux.Handle("POST /api/sessions/{backend}/{session}/background", s.api(s.handleBackground))
	mux.Handle("GET /api/sessions/{backend}/{session}/background-tokens", s.api(s.handleBackgroundTokens))
	mux.Handle("POST /api/places", s.api(s.handlePlaces))
	mux.Handle("GET /api/computers", s.api(s.handleComputers))
	mux.Handle("GET /api/computers/{device}/folders", s.api(s.handleFolders))
	s.memoryRoutes(mux)
	s.modelRoutes(mux)
	s.insightRoutes(mux)
	s.notifyRoutes(mux)
	s.claudeCodeRoutes(mux)
	s.claudeRoutes(mux)
	s.scheduledRoutes(mux)
	s.composeRoutes(mux)
	s.promptRoutes(mux)
	s.partnerRoutes(mux)
	s.userRoutes(mux)
	s.agentRoutes(mux)
	s.adminRoutes(mux)
	mux.HandleFunc("GET /link/telegram", s.handleLinkPage)
	mux.HandleFunc("POST /link/telegram", s.handleLink)
	mux.HandleFunc("GET /device/pair", s.handlePairPage)
	mux.HandleFunc("POST /device/pair", s.handlePair)
	mux.Handle("GET /api/devices", s.api(s.handleDevices))
	mux.Handle("POST /api/devices/{device}/revoke", s.api(s.handleRevokeDevice))
	mux.Handle("POST /api/devices/{device}/dismiss-address", s.api(s.handleDismissAddress))
	mux.Handle("POST /api/devices/{device}/rename", s.api(s.handleRenameDevice))
	if s.deviceAPI != nil {
		mux.Handle("/api/device/", s.deviceAPI)
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

// crossOrigin answers a request whose Origin is not the public URL. The
// message names the fix, because the usual cause is opening the hub at
// another address (an IP, a second host name) than the configured one.
func (s *Server) crossOrigin(w http.ResponseWriter) {
	errorJSON(w, http.StatusForbidden, "Open Talaria at "+s.origin+", or set TALARIA_PUBLIC_URL to this address.")
}

const wrongAddressHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Wrong address - Talaria</title><link rel="stylesheet" href="/static/app.css"></head>
<body class="doc"><main class="page">
<h1>Wrong address</h1>
<p class="err">Open Talaria at <a href="{{.URL}}">{{.Origin}}</a>, or set TALARIA_PUBLIC_URL to this address.</p>
</main></body></html>`

// wrongAddress answers a form post (a full page) sent from another address
// than the public URL with a page that links to the same page at the right one.
func (s *Server) wrongAddress(w http.ResponseWriter, pathAndQuery string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	page := template.Must(template.New("wrong-address").Parse(wrongAddressHTML))
	if err := page.Execute(w, struct{ URL, Origin string }{s.origin + pathAndQuery, s.origin}); err != nil {
		s.log.Debug("render wrong address page", "error", err.Error())
	}
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
			s.crossOrigin(w)
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

// MsgNoModel is the answer to a chat that cannot start because Hermes has
// no model set up.
const MsgNoModel = "No model is set up yet. Pick a model in Settings, then start the chat."

func (s *Server) fail(w http.ResponseWriter, err error) {
	if computerError(w, err) || unknownCommand(w, err) {
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
		errorJSON(w, http.StatusConflict, "Talaria is still working on this chat. Stop it or wait until it is done, then try again")
	case errors.Is(err, hub.ErrHoldFull):
		errorJSON(w, http.StatusTooManyRequests, "Too many messages are waiting for the agent to come back. Send this one again later")
	case errors.Is(err, hub.ErrBackendAway):
		errorJSON(w, http.StatusServiceUnavailable, strings.TrimPrefix(err.Error(), hub.ErrBackendAway.Error()+": "))
	case errors.Is(err, hub.ErrNoModel):
		errorJSON(w, http.StatusConflict, MsgNoModel)
	case errors.Is(err, hub.ErrShuttingDown):
		errorJSON(w, http.StatusServiceUnavailable, "The hub is restarting. Send the message again in a moment")
	case errors.Is(err, hub.ErrTitleInUse):
		errorJSON(w, http.StatusConflict, "The agent did not accept that name. Another chat may already use it")
	case errors.Is(err, hub.ErrAttachment):
		errorJSON(w, http.StatusBadRequest, hub.AttachmentReason(err))
	case errors.Is(err, hub.ErrBadValue):
		msg := strings.TrimPrefix(err.Error(), hub.ErrBadValue.Error()+": ")
		if !strings.Contains(msg, " ") {
			msg = "The agent did not accept that"
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
