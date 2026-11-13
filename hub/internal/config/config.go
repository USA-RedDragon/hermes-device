// Package config holds the hub's configuration.
package config

//go:generate go tool configulator -type Config

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/telegram"
)

// Metrics configures the Prometheus metrics server.
type Metrics struct {
	Enabled  bool   `name:"enabled" default:"false" description:"Enable the metrics server"`
	IPV4Host string `name:"ipv4_host" default:"127.0.0.1" description:"Metrics server IPv4 host"`
	IPV6Host string `name:"ipv6_host" default:"::1" description:"Metrics server IPv6 host"`
	Port     uint16 `name:"port" default:"8081" description:"Metrics server port"`
}

// HTTP configures the web server.
type HTTP struct {
	IPV4Host  string  `name:"ipv4_host" default:"0.0.0.0" description:"HTTP server IPv4 host"`
	IPV6Host  string  `name:"ipv6_host" default:"::" description:"HTTP server IPv6 host"`
	Port      uint16  `name:"port" default:"8080" description:"HTTP server port"`
	PublicURL string  `name:"public_url" required:"true" description:"Public base URL of the hub, such as https://hub.example.com. Sets the OIDC redirect URI (<public_url>/auth/callback), the only allowed Origin and whether cookies are Secure"`
	Metrics   Metrics `name:"metrics"`
}

// OIDC configures login through an OpenID Connect provider (auth.mode oidc).
type OIDC struct {
	IssuerURL        string   `name:"issuer_url" description:"OIDC issuer URL, such as https://auth.example.com/realms/home. Required in auth.mode oidc; setting it makes oidc the default mode"`
	ClientID         string   `name:"client_id" description:"OIDC client ID. Required in auth.mode oidc"`
	ClientSecret     string   `name:"client_secret" secret:"true" description:"OIDC client secret. Required in auth.mode oidc unless client_secret_file is set"`
	ClientSecretFile string   `name:"client_secret_file" description:"File that holds oidc.client_secret. Wins over client_secret"`
	Scopes           []string `name:"scopes" default:"openid,profile,email" description:"Scopes to request. Add the scope that carries groups_claim if the provider needs one (such as groups)"`
	DisplayClaim     string   `name:"display_claim" default:"preferred_username" description:"ID token claim shown as the user's name. Display only: identity is always the sub claim"`
	GroupsClaim      string   `name:"groups_claim" default:"groups" description:"ID token claim that holds the group names that backends[].groups and oidc.allowed_groups list"`
	EmailClaim       string   `name:"email_claim" default:"email" description:"ID token claim with the user's email address, for oidc.allowed_domains and accounts an admin made by email. Used only when email_verified is true"`
	AutoCreate       bool     `name:"auto_create" default:"false" description:"Make an account at the first login of a sub that is not in users[], when its verified email is in allowed_domains or it is in one of allowed_groups. Off: only subs in users[] and accounts an admin made log in"`
	AllowedDomains   []string `name:"allowed_domains" description:"With auto_create: email domains whose verified addresses get an account, such as example.com. * allows any verified email"`
	AllowedGroups    []string `name:"allowed_groups" description:"With auto_create: groups (groups_claim) whose members get an account"`
}

// Session configures the login cookie.
type Session struct {
	Secret     string        `name:"secret" secret:"true" description:"Key for the session and login cookies, at least 32 characters. Empty with no secret_file: the hub makes one and keeps it in its database"`
	SecretFile string        `name:"secret_file" description:"File that holds session.secret. Wins over secret"`
	TTL        time.Duration `name:"ttl" default:"168h" description:"How long a login lasts"`
}

// Store drivers.
const (
	DriverSQLite   = "sqlite"
	DriverPostgres = "postgres"
)

// Store configures the database.
type Store struct {
	Driver     string `name:"driver" default:"sqlite" description:"Database: sqlite (a file, for one replica only) or postgres (for one or more replicas). It holds Telegram links, link tokens, users' last seen groups, the Telegram bot's state and paired devices"`
	Path       string `name:"path" default:"/data/hermes-hub.db" description:"Path of the SQLite database (store.driver sqlite)"`
	DSN        string `name:"dsn" secret:"true" description:"PostgreSQL connection string (store.driver postgres), such as postgres://hub:password@db.example.com:5432/hub. Connect to the database directly, not through a PgBouncer pool in transaction mode: the Telegram leader lock needs its own session"`
	DSNFile    string `name:"dsn_file" description:"File that holds store.dsn. Wins over dsn"`
	ImportPath string `name:"import_path" description:"With store.driver postgres: an SQLite database of an earlier hub to copy into PostgreSQL at startup. The copy runs once; a missing file is skipped"`
}

// Shutdown configures what the hub does after SIGTERM.
type Shutdown struct {
	DrainDelay time.Duration `name:"drain_delay" default:"5s" description:"How long the hub keeps serving after SIGTERM while /readyz fails, so load balancers stop sending it requests"`
	Timeout    time.Duration `name:"timeout" default:"20s" description:"How long the hub then waits for open requests and the Telegram bot to stop. Keep drain_delay plus timeout below the pod's termination grace period"`
}

// defaultHoldTimeout is hold.timeout's default, also for a zero value.
const defaultHoldTimeout = 10 * time.Minute

// Hold configures prompts the hub holds while a backend restarts.
type Hold struct {
	Timeout time.Duration `name:"timeout" default:"10m" description:"How long the hub holds a message while its Hermes backend is restarting or cannot be reached. After that the user is told the message was not sent. 0 is the default"`
}

// Usage configures the Usage page.
type Usage struct {
	PrometheusURL string `name:"prometheus_url" description:"Base URL of a Prometheus query API, such as http://prometheus:9090, that scrapes the metrics of ollama-priority-proxy in front of the local GPU's Ollama. The Usage page then shows how long the GPU was busy. Empty: it does not"`
}

// User maps an OIDC subject to a hub user name.
type User struct {
	Sub  string `name:"sub" description:"OIDC sub claim of the user. Authentik's default sub mode is the hashed user ID"`
	Name string `name:"name" description:"Hub user name, used by backends[].users, ui.default_backends and Telegram links"`
}

// UI configures the web UI. Every user gets the same UI.
type UI struct {
	DefaultBackends []string `name:"default_backends" description:"Default backend per user, as user=backend pairs. A user without one gets their first backend"`
	TimeZone        string   `name:"time_zone" description:"IANA time zone of the times on the Scheduled page, such as Europe/Berlin. Default: the server's zone (the TZ environment variable, else /etc/localtime), else UTC"`
}

// Location is TimeZone's location; UTC for an unknown zone.
func (u UI) Location() *time.Location {
	return location(u.TimeZone)
}

func location(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil || name == "" {
		return time.UTC
	}
	return loc
}

// DefaultBackendMap parses DefaultBackends.
func (u UI) DefaultBackendMap() (map[string]string, error) {
	out := make(map[string]string, len(u.DefaultBackends))
	for _, pair := range u.DefaultBackends {
		user, backend, ok := strings.Cut(pair, "=")
		user, backend = strings.TrimSpace(user), strings.TrimSpace(backend)
		if !ok || user == "" || backend == "" {
			return nil, fmt.Errorf("%w: %q", ErrDefaultBackend, pair)
		}
		out[user] = backend
	}
	return out, nil
}

// Backend is one Hermes gateway Webapp listener.
type Backend struct {
	ID             string          `name:"id" description:"Stable backend ID: lowercase letters, digits and dashes"`
	Name           string          `name:"name" description:"Display name"`
	URL            string          `name:"url" description:"Base URL of the Hermes Webapp listener, such as http://desktop:9120"`
	Provider       string          `name:"provider" description:"Hermes dashboard password provider. Default basic"`
	Username       string          `name:"username" description:"Hermes dashboard user name"`
	Password       string          `name:"password" secret:"true" description:"Hermes dashboard password. Required unless password_file is set"`
	PasswordFile   string          `name:"password_file" description:"File that holds the Hermes dashboard password. Wins over password"`
	Users          []string        `name:"users" description:"Hub user names that may use this backend"`
	Groups         []string        `name:"groups" description:"Hub groups whose members may use this backend"`
	Everyone       bool            `name:"everyone" description:"Every hub user may use this backend. Default false"`
	CWD            string          `name:"cwd" description:"Working directory for new sessions. Default: the gateway's own"`
	ShowReasoning  *bool           `name:"show_reasoning" description:"Turn on reasoning streaming for each session the hub attaches to. Default true"`
	Devices        bool            `name:"devices" description:"Let users pair talaria computers with this backend. Its Hermes needs the talaria device plugin, with plugin setting backend_id equal to this backend's id and the hub's public key. Default false"`
	ACPURL         string          `name:"acp_url" description:"Base URL of this backend's acp-host, such as http://desktop:9130, for editors that connect with talaria acp. Only the hub may reach it. Empty: editors cannot connect"`
	MediaRoots     []string        `name:"media_roots" description:"Absolute folders on this backend's server whose files the hub shows: files a reply names with MEDIA:<path>, browser_screenshot results and images users attached. The hub shows a file only when that chat names it. Empty: only images, in the folders of the talaria device plugin's browser_screenshot (<dir>/cache/screenshots/hermes-device/<session>/<file>) and of attached images (<dir>/cache/images/<file>), under any <dir>. Set it to anchor them to the backend's HERMES_HOME. Other paths, and those the server does not have, are read from the computer or sandbox that runs the chat"`
	ServiceDevices []ServiceDevice `name:"service_devices" description:"Devices the hub knows from this list instead of pairing, such as each user's sandbox pod that runs talaria run. They may use only the exec leg"`
	Honcho         Honcho          `name:"honcho"`
	AgentMessages  *bool           `name:"agent_messages" description:"Let the agents of this backend message each other through the hub with the talaria device plugin's device-agents tools (needs devices). Default: on when users names exactly one user and groups is empty; a backend shared by several people needs true, and then all its chats count as one person's"`
}

// HermesBackendID is the id of the backend that hermes.url adds.
const HermesBackendID = "hermes"

// Hermes is a shortcut for one backend that every hub user may use, so a
// one-person install needs no backends[] list.
type Hermes struct {
	URL          string `name:"url" description:"Base URL of a Hermes Webapp listener, such as http://hermes:9120. Set: the hub adds a backend with id hermes that every hub user may use, in front of backends[]"`
	Name         string `name:"name" default:"Hermes" description:"Display name of that backend"`
	Username     string `name:"username" description:"Hermes dashboard user name of that backend. Required with url"`
	Password     string `name:"password" secret:"true" description:"Hermes dashboard password of that backend. Required with url unless password_file is set"`
	PasswordFile string `name:"password_file" description:"File that holds hermes.password. Wins over password"`
	Devices      bool   `name:"devices" default:"false" description:"Let users pair talaria computers with that backend, as backends[].devices"`
}

// backend is the backend Hermes describes, or false when url is empty.
func (h Hermes) backend() (Backend, bool) {
	if h.URL == "" {
		return Backend{}, false
	}
	return Backend{ID: HermesBackendID, Name: h.Name, URL: h.URL, Username: h.Username, Password: h.Password,
		Everyone: true, Devices: h.Devices}, true
}

// Honcho is where a backend's Hermes keeps what it learned about its user,
// for the Learned tab of the Memory page.
type Honcho struct {
	URL        string `name:"url" description:"Base URL of the Honcho API that this backend's Hermes uses, such as http://honcho.example.svc:8000. Empty: the Memory page has no Learned tab for this backend"`
	Workspace  string `name:"workspace" description:"Honcho workspace of this backend's user. Required with url"`
	UserPeer   string `name:"user_peer" description:"Honcho peer id of the user. Required with url"`
	AIPeer     string `name:"ai_peer" description:"Honcho peer id of Hermes. Default hermes"`
	APIKey     string `name:"api_key" secret:"true" description:"Honcho API key (a JWT scoped to the workspace). The hub never sends it to browsers. Required with url unless api_key_file or api_key_env is set"`
	APIKeyFile string `name:"api_key_file" description:"File that holds the Honcho API key. Wins over api_key_env and api_key"`
	APIKeyEnv  string `name:"api_key_env" description:"Environment variable that holds the Honcho API key. Wins over api_key"`
}

// Claude configures the Claude subscription accounts that the
// Claude Hermes plugin runs.
type Claude struct {
	Admins   []string `name:"admins" description:"Who may sign the Claude accounts of a backend in from the hub, as user=backend pairs. They see the sign-in controls and get the sign-in notices; the backend's other users see only usage. Empty: nobody"`
	TimeZone string   `name:"time_zone" description:"IANA time zone of the reset times of Claude usage windows and of the days of Claude Code usage. Default: ui.time_zone"`
}

// AdminMap parses Admins: the Claude admins of each backend.
func (c Claude) AdminMap() (map[string][]string, error) {
	out := map[string][]string{}
	for _, pair := range c.Admins {
		user, backend, ok := strings.Cut(pair, "=")
		user, backend = strings.TrimSpace(user), strings.TrimSpace(backend)
		if !ok || user == "" || backend == "" {
			return nil, fmt.Errorf("%w: %q", ErrClaudeAdmin, pair)
		}
		out[backend] = append(out[backend], user)
	}
	return out, nil
}

// Location is TimeZone's location; UTC for an unknown zone.
func (c Claude) Location() *time.Location {
	return location(c.TimeZone)
}

func (c *Config) validateClaude(known userNames) error {
	admins, err := c.Claude.AdminMap()
	if err != nil {
		return err
	}
	backends := map[string]bool{}
	for _, b := range c.Backends {
		backends[b.ID] = true
	}
	for backend, users := range admins {
		if !backends[backend] {
			return fmt.Errorf("%w: claude.admins names unknown backend %q", ErrClaudeAdmin, backend)
		}
		for _, u := range users {
			if !known.known(u) {
				return fmt.Errorf("%w: claude.admins names unknown user %q", ErrUser, u)
			}
		}
	}
	if c.Claude.TimeZone == "" {
		c.Claude.TimeZone = c.UI.TimeZone
	}
	if _, err := time.LoadLocation(c.Claude.TimeZone); err != nil {
		return fmt.Errorf("%w: %q", ErrClaudeTimeZone, c.Claude.TimeZone)
	}
	return nil
}

// Device configures hermes-device: computers that users pair with one of
// their backends, so Hermes runs commands and editors on them.
type Device struct {
	Enabled         bool          `name:"enabled" default:"false" description:"Serve the talaria device API under /api/device/ and the pairing page /device/pair. Backends also need backends[].devices"`
	SigningKey      string        `name:"signing_key" secret:"true" description:"Ed25519 private key that signs the hub's device assertions: a PEM PRIVATE KEY block (openssl genpkey -algorithm ed25519) or the 32-byte seed in base64. Required with device.enabled unless signing_key_file is set"`
	SigningKeyFile  string        `name:"signing_key_file" description:"File that holds device.signing_key. Wins over signing_key"`
	PairingTTL      time.Duration `name:"pairing_ttl" default:"10m" description:"How long a pairing code from talaria setup or talaria login stays valid"`
	PollInterval    time.Duration `name:"poll_interval" default:"5s" description:"How often a device may poll while it waits for the user to confirm pairing"`
	RecheckInterval time.Duration `name:"recheck_interval" default:"10s" description:"How often the hub checks that each open device connection's device is not revoked and that its user may still use its backend. A revoke closes the device's connections on the replica that served it at once, and on other replicas within this time"`
	TrustedProxies  []string      `name:"trusted_proxies" description:"CIDRs of proxies in front of the hub, such as 10.0.0.0/8. For a request from one of them the hub takes the device's address from X-Forwarded-For. Empty: the address of the TCP peer"`
}

// ServiceDevice is a device defined in the config, pinned to its backend.
type ServiceDevice struct {
	ID        string `name:"id" description:"Device id the backend sees: letters, digits, _ and -, at most 64, not starting with d_ (paired devices use that)"`
	Name      string `name:"name" description:"Name shown for the device. Default: the id"`
	Token     string `name:"token" secret:"true" description:"The device's token, as talaria stores it: hdt_ and at least 32 more characters. The hub keeps only its SHA-256. Required unless token_file is set"`
	TokenFile string `name:"token_file" description:"File that holds the device's token. Wins over token"`
}

// Telegram is telegram.Config under its own name: configulator names its
// generated shadow types by bare type name, so a nested type called Config
// would collide with this package's Config.
type Telegram telegram.Config

// Config is the hub's configuration.
type Config struct {
	LogLevel string    `name:"log_level" default:"info" description:"Log level: debug, info, warn or error"`
	HTTP     HTTP      `name:"http"`
	OIDC     OIDC      `name:"oidc"`
	Session  Session   `name:"session"`
	UI       UI        `name:"ui"`
	Store    Store     `name:"store"`
	Shutdown Shutdown  `name:"shutdown"`
	Hold     Hold      `name:"hold"`
	Usage    Usage     `name:"usage"`
	Auth     Auth      `name:"auth"`
	Users    []User    `name:"users" description:"Identities from the identity provider or proxy and their hub user names: each OIDC sub (auth.mode oidc) or proxy identity (auth.mode proxy). Not used in auth.mode local, whose accounts live in the database. Only settable in the config file"`
	Hermes   Hermes    `name:"hermes"`
	Backends []Backend `name:"backends" description:"Hermes backends and who may use them. Only settable in the config file; for one backend, hermes.* works from environment variables too"`
	Telegram Telegram  `name:"telegram"`
	Device   Device    `name:"device"`
	Notify   Notify    `name:"notify"`
	Claude   Claude    `name:"claude"`
}

// Validation errors.
var (
	ErrInvalidLogLevel = errors.New("invalid log level")
	ErrShortSecret     = errors.New("session.secret must be at least 32 characters")
	ErrPublicURL       = errors.New("http.public_url must be an absolute http or https URL with no path")
	ErrBackend         = errors.New("invalid backend")
	ErrUser            = errors.New("invalid user")
	ErrDefaultBackend  = errors.New("ui.default_backends entries must be user=backend")
	ErrStoreDriver     = errors.New("store.driver must be sqlite or postgres")
	ErrStoreDSN        = errors.New("store.dsn is required when store.driver is postgres")
	ErrImportPath      = errors.New("store.import_path needs store.driver postgres")
	ErrDuration        = errors.New("shutdown.drain_delay, shutdown.timeout and hold.timeout must not be negative")
	ErrDevice          = errors.New("invalid device settings")
	ErrPrometheusURL   = errors.New("usage.prometheus_url must be an absolute http or https URL")
	ErrClaudeAdmin     = errors.New("claude.admins entries must be user=backend with a known backend")
	ErrClaudeTimeZone  = errors.New("claude.time_zone is not a known time zone")
	ErrUITimeZone      = errors.New("ui.time_zone is not a known time zone")
)

var backendIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func webURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, false
	}
	return u, true
}

// UserMap maps each sub to its hub user name.
func (c *Config) UserMap() map[string]string {
	out := make(map[string]string, len(c.Users))
	for _, u := range c.Users {
		out[u.Sub] = u.Name
	}
	return out
}

// userNames checks the hub user names that settings name. With a fixed
// users[] list (and no accounts made at login) each must be in it; else
// accounts in the database may hold them, so only their form is checked.
type userNames struct {
	fixed bool
	names map[string]bool
}

func (n userNames) known(name string) bool {
	if n.fixed {
		return n.names[name]
	}
	return name != "" && !strings.ContainsAny(name, "=, ")
}

func (c *Config) validateUsers() error {
	subs, names := map[string]bool{}, map[string]bool{}
	for i, u := range c.Users {
		if u.Sub == "" || u.Name == "" || strings.ContainsAny(u.Name, "=, ") {
			return fmt.Errorf("%w: users[%d] needs sub and a name without spaces, commas or =", ErrUser, i)
		}
		if subs[u.Sub] || names[u.Name] {
			return fmt.Errorf("%w: users[%d] repeats a sub or name", ErrUser, i)
		}
		subs[u.Sub], names[u.Name] = true, true
	}
	known := userNames{names: names,
		fixed: c.Auth.Mode == AuthOIDC && !c.OIDC.AutoCreate || c.Auth.Mode == AuthProxy && !c.Auth.Proxy.AutoCreate && len(c.Users) > 0}
	defaults, err := c.UI.DefaultBackendMap()
	if err != nil {
		return err
	}
	for user := range defaults {
		if !known.known(user) {
			return fmt.Errorf("%w: ui.default_backends names unknown user %q", ErrUser, user)
		}
	}
	for i, b := range c.Backends {
		for _, user := range b.Users {
			if !known.known(user) {
				return fmt.Errorf("%w: backends[%d].users names unknown user %q", ErrUser, i, user)
			}
		}
	}
	for _, user := range c.Auth.Admins {
		if !known.known(user) {
			return fmt.Errorf("%w: auth.admins names unknown user %q", ErrUser, user)
		}
	}
	if c.UI.TimeZone == "" {
		c.UI.TimeZone = ServerTimeZone()
	}
	if _, err := time.LoadLocation(c.UI.TimeZone); err != nil {
		return fmt.Errorf("%w: %q", ErrUITimeZone, c.UI.TimeZone)
	}
	return c.validateClaude(known)
}

func (c *Config) validateStore() error {
	if c.Store.Driver == "" {
		c.Store.Driver = DriverSQLite
	}
	switch c.Store.Driver {
	case DriverSQLite:
		if c.Store.ImportPath != "" {
			return ErrImportPath
		}
	case DriverPostgres:
		if c.Store.DSN == "" {
			return ErrStoreDSN
		}
	default:
		return ErrStoreDriver
	}
	return nil
}

// Validate checks what tags can't and fills backend defaults.
func (c *Config) Validate() error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return ErrInvalidLogLevel
	}
	if err := c.readSecretFiles(); err != nil {
		return err
	}
	if err := c.validateAuth(); err != nil {
		return err
	}
	if err := c.addHermes(); err != nil {
		return err
	}
	if err := c.validateUsers(); err != nil {
		return err
	}
	if err := telegram.Config(c.Telegram).Validate(); err != nil {
		return err
	}
	if err := c.validateStore(); err != nil {
		return err
	}
	if c.Hold.Timeout == 0 {
		c.Hold.Timeout = defaultHoldTimeout
	}
	if c.Shutdown.DrainDelay < 0 || c.Shutdown.Timeout < 0 || c.Hold.Timeout < 0 {
		return ErrDuration
	}
	if c.Session.Secret != "" && len(c.Session.Secret) < 32 {
		return ErrShortSecret
	}
	if c.HTTP.PublicURL != "" {
		u, ok := webURL(c.HTTP.PublicURL)
		if !ok || strings.Trim(u.Path, "/") != "" {
			return ErrPublicURL
		}
		c.HTTP.PublicURL = strings.TrimRight(c.HTTP.PublicURL, "/")
	}
	if c.Usage.PrometheusURL != "" {
		if _, ok := webURL(c.Usage.PrometheusURL); !ok {
			return ErrPrometheusURL
		}
		c.Usage.PrometheusURL = strings.TrimRight(c.Usage.PrometheusURL, "/")
	}
	if err := c.validateBackends(); err != nil {
		return err
	}
	if err := c.validateNotify(); err != nil {
		return err
	}
	return c.validateDevices()
}

// addHermes puts the backend of hermes.url in front of backends[].
func (c *Config) addHermes() error {
	b, ok := c.Hermes.backend()
	if !ok {
		return nil
	}
	for _, other := range c.Backends {
		if other.ID == HermesBackendID {
			return fmt.Errorf("%w: hermes.url adds backend id %s, which backends[] already has", ErrBackend, HermesBackendID)
		}
	}
	if b.Username == "" || b.Password == "" {
		return fmt.Errorf("%w: hermes.url needs hermes.username and hermes.password (or password_file)", ErrBackend)
	}
	c.Backends = append([]Backend{b}, c.Backends...)
	return nil
}

func (c *Config) validateBackends() error {
	seen, seenDevices := map[string]bool{}, map[string]bool{}
	for i := range c.Backends {
		b := &c.Backends[i]
		if !backendIDPattern.MatchString(b.ID) || seen[b.ID] {
			return fmt.Errorf("%w: backends[%d].id %q must be unique lowercase letters, digits and dashes", ErrBackend, i, b.ID)
		}
		seen[b.ID] = true
		if _, ok := webURL(b.URL); !ok {
			return fmt.Errorf("%w: backends[%d].url must be an http or https URL", ErrBackend, i)
		}
		b.URL = strings.TrimRight(b.URL, "/")
		if b.Username == "" || b.Password == "" {
			return fmt.Errorf("%w: backends[%d] needs username and password", ErrBackend, i)
		}
		if b.Name == "" {
			b.Name = b.ID
		}
		if b.Provider == "" {
			b.Provider = "basic"
		}
		if b.ShowReasoning == nil {
			on := true
			b.ShowReasoning = &on
		}
		if err := validateServiceDevices(i, b, seenDevices); err != nil {
			return err
		}
		for j, r := range b.MediaRoots {
			if !strings.HasPrefix(r, "/") || path.Clean(r) != r || r == "/" {
				return fmt.Errorf("%w: backends[%d].media_roots[%d] must be a clean absolute folder other than /", ErrBackend, i, j)
			}
		}
		if err := b.Honcho.validate(i); err != nil {
			return err
		}
		if b.ACPURL != "" {
			if _, ok := webURL(b.ACPURL); !ok {
				return fmt.Errorf("%w: backends[%d].acp_url must be an http or https URL", ErrBackend, i)
			}
			b.ACPURL = strings.TrimRight(b.ACPURL, "/")
		}
	}
	return nil
}

func (c *Config) validateDevices() error {
	d := &c.Device
	if d.PairingTTL < 0 || d.PollInterval < 0 || d.RecheckInterval < 0 {
		return fmt.Errorf("%w: device.pairing_ttl, poll_interval and recheck_interval must not be negative", ErrDevice)
	}
	if d.PairingTTL == 0 {
		d.PairingTTL = defaultPairingTTL
	}
	if d.PollInterval == 0 {
		d.PollInterval = defaultPollInterval
	}
	if d.RecheckInterval == 0 {
		d.RecheckInterval = defaultRecheckInterval
	}
	if _, err := d.Proxies(); err != nil {
		return err
	}
	if !d.Enabled {
		return nil
	}
	if _, err := d.PrivateKey(); err != nil {
		return err
	}
	return nil
}
