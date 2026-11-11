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

	"github.com/USA-RedDragon/hermes-hub/internal/telegram"
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

// OIDC configures login through the OpenID Connect provider (Authentik).
type OIDC struct {
	IssuerURL    string   `name:"issuer_url" required:"true" description:"OIDC issuer URL, such as https://auth.example.com/application/o/hermes-hub/"`
	ClientID     string   `name:"client_id" required:"true" description:"OIDC client ID"`
	ClientSecret string   `name:"client_secret" required:"true" secret:"true" description:"OIDC client secret"`
	Scopes       []string `name:"scopes" default:"openid,profile,email" description:"Scopes to request. Authentik's profile scope carries preferred_username and groups"`
	DisplayClaim string   `name:"display_claim" default:"preferred_username" description:"ID token claim shown as the user's name. Display only: identity is always the sub claim"`
	GroupsClaim  string   `name:"groups_claim" default:"groups" description:"ID token claim that holds the group names that backends[].groups lists"`
}

// Session configures the login cookie.
type Session struct {
	Secret string        `name:"secret" required:"true" secret:"true" description:"Key for the session and login cookies. At least 32 characters"`
	TTL    time.Duration `name:"ttl" default:"168h" description:"How long a login lasts"`
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

// User maps an OIDC subject to a hub user name.
type User struct {
	Sub  string `name:"sub" description:"OIDC sub claim of the user. Authentik's default sub mode is the hashed user ID"`
	Name string `name:"name" description:"Hub user name, used by backends[].users, ui.default_backends and Telegram links"`
}

// UI configures the web UI. Every user gets the same UI.
type UI struct {
	DefaultBackends []string `name:"default_backends" description:"Default backend per user, as user=backend pairs. A user without one gets their first backend"`
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
	Password       string          `name:"password" secret:"true" description:"Hermes dashboard password"`
	Users          []string        `name:"users" description:"Hub user names that may use this backend"`
	Groups         []string        `name:"groups" description:"Hub groups whose members may use this backend"`
	CWD            string          `name:"cwd" description:"Working directory for new sessions. Default: the gateway's own"`
	ShowReasoning  *bool           `name:"show_reasoning" description:"Turn on reasoning streaming for each session the hub attaches to. Default true"`
	Devices        bool            `name:"devices" description:"Let users pair hermes-device devices with this backend. Its Hermes needs the hermes-device plugin, with plugin setting backend_id equal to this backend's id and the hub's public key. Default false"`
	ACPURL         string          `name:"acp_url" description:"Base URL of this backend's acp-host, such as http://desktop:9130, for editors that connect with hermes-device acp. Only the hub may reach it. Empty: editors cannot connect"`
	MediaRoots     []string        `name:"media_roots" description:"Absolute folders on this backend's server whose files the hub shows: files a reply names with MEDIA:<path>, browser_screenshot results and images users attached. The hub shows a file only when that chat names it. Empty: only images, in the folders of hermes-device's browser_screenshot (<dir>/cache/screenshots/hermes-device/<session>/<file>) and of attached images (<dir>/cache/images/<file>), under any <dir>. Set it to anchor them to the backend's HERMES_HOME"`
	ServiceDevices []ServiceDevice `name:"service_devices" description:"Devices the hub knows from this list instead of pairing, such as each user's sandbox pod that runs hermes-device run. They may use only the exec leg"`
}

// Device configures hermes-device: computers that users pair with one of
// their backends, so Hermes runs commands and editors on them.
type Device struct {
	Enabled         bool          `name:"enabled" default:"false" description:"Serve the hermes-device API under /api/device/ and the pairing page /device/pair. Backends also need backends[].devices"`
	SigningKey      string        `name:"signing_key" secret:"true" description:"Ed25519 private key that signs the hub's device assertions: a PEM PRIVATE KEY block (openssl genpkey -algorithm ed25519) or the 32-byte seed in base64. Required with device.enabled unless signing_key_file is set"`
	SigningKeyFile  string        `name:"signing_key_file" description:"File that holds device.signing_key. Wins over signing_key"`
	PairingTTL      time.Duration `name:"pairing_ttl" default:"10m" description:"How long a pairing code from hermes-device login stays valid"`
	PollInterval    time.Duration `name:"poll_interval" default:"5s" description:"How often a device may poll while it waits for the user to confirm pairing"`
	RecheckInterval time.Duration `name:"recheck_interval" default:"10s" description:"How often the hub checks that each open device connection's device is not revoked and that its user may still use its backend. A revoke closes the device's connections on the replica that served it at once, and on other replicas within this time"`
	TrustedProxies  []string      `name:"trusted_proxies" description:"CIDRs of proxies in front of the hub, such as 10.0.0.0/8. For a request from one of them the hub takes the device's address from X-Forwarded-For. Empty: the address of the TCP peer"`
}

// ServiceDevice is a device defined in the config, pinned to its backend.
type ServiceDevice struct {
	ID    string `name:"id" description:"Device id the backend sees: letters, digits, _ and -, at most 64, not starting with d_ (paired devices use that)"`
	Name  string `name:"name" description:"Name shown for the device. Default: the id"`
	Token string `name:"token" secret:"true" description:"The device's token, as hermes-device stores it: hdt_ and at least 32 more characters. The hub keeps only its SHA-256"`
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
	Users    []User    `name:"users" description:"Who may log in: each OIDC sub and its hub user name. A login with any other sub is refused. Only settable in the config file"`
	Backends []Backend `name:"backends" description:"Hermes backends and who may use them. Only settable in the config file"`
	Telegram Telegram  `name:"telegram"`
	Device   Device    `name:"device"`
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
	defaults, err := c.UI.DefaultBackendMap()
	if err != nil {
		return err
	}
	for user := range defaults {
		if !names[user] {
			return fmt.Errorf("%w: ui.default_backends names unknown user %q", ErrUser, user)
		}
	}
	for i, b := range c.Backends {
		for _, user := range b.Users {
			if !names[user] {
				return fmt.Errorf("%w: backends[%d].users names unknown user %q", ErrUser, i, user)
			}
		}
	}
	return nil
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
	if err := c.validateBackends(); err != nil {
		return err
	}
	return c.validateDevices()
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
