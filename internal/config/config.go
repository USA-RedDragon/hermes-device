// Package config holds the configuration of every talaria subcommand.
package config

//go:generate go tool configulator -type Config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/mcp"
)

// Hub is where the device connects.
type Hub struct {
	URL       string `name:"url" description:"Base URL of the Talaria hub, for example https://hub.example.com. Needed by login, run, chat and acp"`
	Token     string `name:"token" secret:"true" description:"Device token for run, instead of the one login stored, for a service device such as the sandbox pod. Wins over hub.token-file and the token store"`
	TokenFile string `name:"token-file" description:"File holding only the device token for run, for example a mounted Kubernetes secret. Must not be readable by others. Wins over the token store"`
}

// Store is where the device keeps its token and daemon state.
type Store struct {
	Keyring   bool   `name:"keyring" default:"true" description:"Keep the device token in the OS keyring when one is available"`
	TokenFile string `name:"token-file" description:"File (mode 0600) for the device token when the keyring is off or unavailable. Defaults to token in the state directory"`
	StateDir  string `name:"state-dir" description:"Directory for the token file and the daemon's state file. Defaults to talaria in the user config directory"`
}

// Device is what the run daemon lets the agent use on this device.
type Device struct {
	Roots        []string      `name:"roots" description:"Absolute directories commands may run in. A command whose working directory is outside all of them is refused. Defaults to the home directory"`
	TempDir      string        `name:"tmp-dir" description:"Directory Hermes uses on this device for its shell snapshot files. Defaults to the system temp directory"`
	Shell        string        `name:"shell" default:"bash" description:"Shell used to run commands"`
	ResumeBuffer int           `name:"resume-buffer" default:"1048576" description:"Most output bytes per command that run keeps for a resume of the exec leg until the plugin confirms them. Older output of a command is dropped, and the plugin shows the gap"`
	MaxGrace     time.Duration `name:"max-grace" default:"10m" description:"Longest time run keeps commands running after the exec leg drops, waiting for a resume. The plugin's grace_seconds applies when it is shorter"`
	// CommandMemoryMax is parsed by CommandMemory.
	CommandMemoryMax string `name:"command-memory-max" default:"auto" description:"Memory cap of each agent command, as bytes or with a K, M, G or T suffix (powers of 1024), for example 4G. On Linux with a systemd user manager, run starts each command in a systemd scope of its own with this MemoryMax, so the kernel kills only a command that uses more, and the agent is told so. auto is 75% of the MemoryMax of the cgroup that run runs in (the talaria service), or 4G when that has no limit. 0 or off runs commands in run's own cgroup, without a cap of their own. No effect on macOS"`
}

// Gateway is the run daemon's loopback splice that chat points the local
// Hermes TUI at.
type Gateway struct {
	Listen string `name:"listen" default:"127.0.0.1:0" description:"Loopback address of the chat splice. Port 0 picks a free port"`
	Splice bool   `name:"splice" default:"true" description:"Run the chat splice in run. false (or run --no-splice) runs only the exec leg: no listener, no daemon state file and no whoami call, for a headless service device such as the sandbox pod"`
}

// Chat runs the local Hermes TUI.
type Chat struct {
	Managed    bool   `name:"managed" default:"true" description:"On Linux, run the exact Hermes build the server runs: run fetches it from the server's container image by digest when the server reports one, verifies it and keeps it in chat.managed-dir. false, or no image from the server, runs chat.hermes instead"`
	ManagedDir string `name:"managed-dir" description:"Directory for the managed Hermes builds and their own Hermes home. Defaults to talaria/hermes in the user cache directory"`
	Hermes     string `name:"hermes" default:"hermes" description:"Local Hermes launcher when chat.managed is off or the server reports no image. It must be the same release as the server's Hermes"`
}

// ACPHost is the in-pod ACP process host.
type ACPHost struct {
	Listen           string `name:"listen" default:":9130" description:"Address the ACP host listens on. Only the Talaria hub may reach it"`
	BackendID        string `name:"backend-id" description:"This backend's id, the aud the hub puts in its assertions. Needed by acp-host"`
	HubPublicKey     string `name:"hub-public-key" description:"Hub Ed25519 public keys: PEM PUBLIC KEY blocks or base64 lines of the raw 32 bytes"`
	HubPublicKeyFile string `name:"hub-public-key-file" description:"File holding the hub public keys, in the same format as hub-public-key"`
	Hermes           string `name:"hermes" default:"hermes" description:"Hermes launcher that the host runs as <hermes> acp for each connection"`
	Placeholder      string `name:"placeholder" description:"Server directory that stands in for the device directory in ACP session cwds. Must match the device plugin's placeholder. Needed by acp-host"`
	MaxProcesses     int    `name:"max-processes" default:"8" description:"Most hermes acp processes the host runs at once"`
	BindingsFile     string `name:"bindings-file" description:"Bindings file the device plugin writes. Defaults to device/bindings.json in $HERMES_HOME"`
	GatewayHealthURL string `name:"gateway-health-url" default:"http://127.0.0.1:9120/api/health" description:"Health URL of the pod's Hermes gateway. The host starts hermes acp only while it answers, because hermes acp starts a gateway of its own when none runs. Empty turns the check off"`
}

// Browser is Firefox browser control (docs/browser.md).
type Browser struct {
	Enabled        bool     `name:"enabled" default:"false" description:"Serve the Hermes Firefox extension on a private socket in the state directory and offer browser control to the plugin, so the agent can use tabs in its tab group. Run talaria browser install once"`
	AllowHosts     []string `name:"allow-hosts" description:"Loopback or private network host names the agent may open. All other loopback, private, link-local and CGNAT addresses are blocked in agent tabs"`
	ConfirmHosts   []string `name:"confirm-hosts" description:"Sites where every click and every Enter or Space key press asks the user each time and scripts may only read. Host names; each covers its subdomains. The plugin cannot change this list"`
	AutologinHosts []string `name:"autologin-hosts" description:"Exact host names where browser_login may submit the login form that Firefox fills from its own saved login, so a scheduled job can sign in without the agent seeing the password. Only https pages whose top-level host is listed; no subdomains. Scripts are refused there, and clicks, typing, key presses and screenshots on its login pages. Example: login.w3.ibm.com. The plugin cannot change this list"`
	StartFirefox   bool     `name:"start-firefox" default:"true" description:"Let the agent start Firefox, with the user's default profile, in the user's graphical session when the extension is not connected, so a scheduled job works after Firefox was closed. The sandbox image turns it off"`
	FirefoxCommand string   `name:"firefox-command" default:"firefox" description:"Command that starts Firefox for start-firefox: a name on PATH or an absolute path"`
}

// MCP is the MCP servers the agent may use on this device
// (docs/protocol.md section 4.9).
type MCP struct {
	Servers     map[string]MCPServer `name:"servers" description:"MCP servers the agent may use on this device, by name: lowercase letters, digits, - and _, at most 32 characters. Each one runs on this device with stdio, started on first use. Only servers listed here can run. Config file only"`
	IdleTimeout time.Duration        `name:"idle-timeout" default:"1h" description:"Stop an MCP server after this long without a request. It starts again on the next request, without its earlier state. 0 keeps it running until run stops"`
}

// MCPServer is one MCP server that runs on this device.
type MCPServer struct {
	Command string            `name:"command" description:"Program to run, a path or a name looked up in the daemon's PATH. Needed"`
	Args    []string          `name:"args" description:"Arguments of the program"`
	Env     map[string]string `name:"env" secret:"true" description:"Environment variables added to the daemon's environment for this server"`
	Dir     string            `name:"dir" description:"Absolute working directory. Defaults to $HOME"`
	// IdleTimeout overrides mcp.idle-timeout for this server; nil uses it.
	IdleTimeout *time.Duration `name:"idle-timeout" description:"Stop this server after this long without a request, instead of mcp.idle-timeout. 0 keeps it running until run stops"`
}

// Reconnect controls the backoff between connection attempts.
type Reconnect struct {
	MinBackoff time.Duration `name:"min-backoff" default:"1s" description:"First delay after a lost connection"`
	MaxBackoff time.Duration `name:"max-backoff" default:"30s" description:"Longest delay between attempts"`
}

// Update is how run keeps talaria up to date (README, Updates).
type Update struct {
	Auto     bool          `name:"auto" default:"true" description:"Let run install new releases by itself: it checks GitHub, verifies the release like talaria update, waits until no command, browser operation, MCP call or chat runs, and exits so the service restarts it. Only when the talaria service runs it and the device token comes from login, so never on a service device that uses hub.token or hub.token-file. The sandbox and device images turn it off"`
	Interval time.Duration `name:"interval" default:"6h" description:"Time between release checks of run, with up to 10% jitter. At least 1h"`
}

// Usage is the Claude Code token count that run and acp-host send to the
// hub (docs/protocol.md section 7).
type Usage struct {
	Enabled           *bool    `name:"enabled" description:"Count the tokens Claude Code used here and send the counts to the hub: only token counts, model names, account ids, the signed-in account's email and project folder names, never what was said. Unset: on for run, except in a container, and on for acp-host"`
	ClaudeDirs        []string `name:"claude-dirs" description:"More Claude Code config directories (CLAUDE_CONFIG_DIR values) to count, besides $CLAUDE_CONFIG_DIR or ~/.claude"`
	HermesAccountsDir string   `name:"hermes-accounts-dir" description:"acp-host: directory of the per-account Claude Code directories. Defaults to $CLAUDE_ACCOUNTS_ROOT, else claude in $HERMES_HOME"`
	StateFile         string   `name:"state-file" description:"File that keeps what was counted across restarts. Defaults to claude-usage.gob in the state directory for run, and device/claude-usage.gob in $HERMES_HOME for acp-host"`
}

// Config is the configuration of every subcommand.
type Config struct {
	LogLevel     string        `name:"log-level" default:"info" description:"Log level: debug, info, warn or error"`
	PingInterval time.Duration `name:"ping-interval" default:"25s" description:"Interval of WebSocket pings on every leg. Must be from 5s to 30s"`
	Hub          Hub           `name:"hub"`
	Store        Store         `name:"store"`
	Device       Device        `name:"device"`
	Gateway      Gateway       `name:"gateway"`
	Chat         Chat          `name:"chat"`
	ACPHost      ACPHost       `name:"acp-host"`
	Reconnect    Reconnect     `name:"reconnect"`
	Browser      Browser       `name:"browser"`
	MCP          MCP           `name:"mcp"`
	Update       Update        `name:"update"`
	Usage        Usage         `name:"usage"`
}

// Validation errors.
var (
	ErrInvalidLogLevel = errors.New("invalid log level")
	ErrInvalidHubURL   = errors.New("hub.url must be an http:// or https:// URL with a host")
	ErrRelativePath    = errors.New("must be an absolute path")
	ErrBackoff         = errors.New("reconnect.min-backoff must be positive and not above reconnect.max-backoff")
	ErrPingInterval    = errors.New("ping-interval must be from 5s to 30s")
	ErrNotLoopback     = errors.New("gateway.listen must be a loopback address")
	ErrMissing         = errors.New("missing setting")
	ErrMaxProcesses    = errors.New("acp-host.max-processes must be at least 1")
	ErrResume          = errors.New("device.resume-buffer must be at least 4096 and device.max-grace must not be negative")
	ErrMCPServer       = errors.New("invalid mcp.servers entry")
	ErrUpdateInterval  = errors.New("update.interval must be at least 1h")
	ErrMemorySize      = errors.New("device.command-memory-max must be auto, off, 0, or a size such as 4G")
)

// Validate checks what the tags can't. Settings only some subcommands need
// are checked by the For* methods.
func (c Config) Validate() error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return ErrInvalidLogLevel
	}
	if c.PingInterval < 5*time.Second || c.PingInterval > 30*time.Second {
		return ErrPingInterval
	}
	if c.Hub.URL != "" {
		if err := checkHubURL(c.Hub.URL); err != nil {
			return err
		}
	}
	for i, r := range c.Device.Roots {
		if !filepath.IsAbs(r) {
			return fmt.Errorf("device.roots[%d] %q: %w", i, r, ErrRelativePath)
		}
	}
	for name, p := range map[string]string{
		"device.tmp-dir": c.Device.TempDir, "store.token-file": c.Store.TokenFile, "store.state-dir": c.Store.StateDir,
		"hub.token-file":       c.Hub.TokenFile,
		"acp-host.placeholder": c.ACPHost.Placeholder, "acp-host.bindings-file": c.ACPHost.BindingsFile,
		"chat.managed-dir":          c.Chat.ManagedDir,
		"usage.hermes-accounts-dir": c.Usage.HermesAccountsDir, "usage.state-file": c.Usage.StateFile,
	} {
		if p != "" && !filepath.IsAbs(p) {
			return fmt.Errorf("%s %q: %w", name, p, ErrRelativePath)
		}
	}
	for i, d := range c.Usage.ClaudeDirs {
		if !filepath.IsAbs(d) {
			return fmt.Errorf("usage.claude-dirs[%d] %q: %w", i, d, ErrRelativePath)
		}
	}
	if c.Reconnect.MinBackoff <= 0 || c.Reconnect.MinBackoff > c.Reconnect.MaxBackoff {
		return ErrBackoff
	}
	if c.ACPHost.MaxProcesses < 1 {
		return ErrMaxProcesses
	}
	if c.Device.ResumeBuffer < 4096 || c.Device.MaxGrace < 0 {
		return ErrResume
	}
	if _, _, err := c.Device.CommandMemory(); err != nil {
		return err
	}
	if c.Update.Interval < time.Hour {
		return ErrUpdateInterval
	}
	if err := c.MCP.validate(); err != nil {
		return err
	}
	return checkLoopback(c.Gateway.Listen)
}

func checkHubURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ErrInvalidHubURL
	}
	return nil
}

func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("gateway.listen %q: %w", addr, err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return ErrNotLoopback
	}
	return nil
}

// ForHub checks the settings every subcommand that talks to the hub needs.
func (c Config) ForHub() error {
	if c.Hub.URL == "" {
		return fmt.Errorf("%w: hub.url", ErrMissing)
	}
	return nil
}

// ForRun checks the settings the run daemon needs.
func (c Config) ForRun() error {
	if err := c.ForHub(); err != nil {
		return err
	}
	return nil
}

// ErrNoHomeRoot is returned when device.roots is unset and the home
// directory cannot stand in for it.
var ErrNoHomeRoot = errors.New("device.roots is unset and the home directory cannot be the default; set device.roots")

// Roots returns device.roots, or the home directory that home returns when
// device.roots is unset. A home of / is refused: it would allow every
// directory.
func (c Config) Roots(home func() (string, error)) ([]string, error) {
	if len(c.Device.Roots) > 0 {
		return c.Device.Roots, nil
	}
	h, err := home()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoHomeRoot, err)
	}
	if !filepath.IsAbs(h) || filepath.Clean(h) == "/" {
		return nil, fmt.Errorf("%w: home is %q", ErrNoHomeRoot, h)
	}
	return []string{filepath.Clean(h)}, nil
}

// ForACPHost checks the settings acp-host needs.
func (c Config) ForACPHost() error {
	switch {
	case c.ACPHost.BackendID == "":
		return fmt.Errorf("%w: acp-host.backend-id", ErrMissing)
	case c.ACPHost.Placeholder == "":
		return fmt.Errorf("%w: acp-host.placeholder", ErrMissing)
	case c.ACPHost.HubPublicKey == "" && c.ACPHost.HubPublicKeyFile == "":
		return fmt.Errorf("%w: acp-host.hub-public-key or acp-host.hub-public-key-file", ErrMissing)
	}
	return nil
}

func (m MCP) validate() error {
	if m.IdleTimeout < 0 {
		return fmt.Errorf("%w: mcp.idle-timeout must not be negative", ErrMCPServer)
	}
	for name, srv := range m.Servers {
		switch {
		case !mcp.NameRE.MatchString(name):
			return fmt.Errorf("%w: name %q must be lowercase letters, digits, - and _, at most 32 characters", ErrMCPServer, name)
		case strings.TrimSpace(srv.Command) == "":
			return fmt.Errorf("%w: mcp.servers.%s.command is empty", ErrMCPServer, name)
		case srv.Dir != "" && !filepath.IsAbs(srv.Dir):
			return fmt.Errorf("%w: mcp.servers.%s.dir %q: %w", ErrMCPServer, name, srv.Dir, ErrRelativePath)
		case srv.IdleTimeout != nil && *srv.IdleTimeout < 0:
			return fmt.Errorf("%w: mcp.servers.%s.idle-timeout must not be negative", ErrMCPServer, name)
		}
		for k := range srv.Env {
			if k == "" || strings.ContainsAny(k, "=\x00") {
				return fmt.Errorf("%w: mcp.servers.%s.env has an invalid name %q", ErrMCPServer, name, k)
			}
		}
	}
	return nil
}

// CommandMemoryAuto is the device.command-memory-max value that takes the
// cap from the daemon's own memory limit.
const CommandMemoryAuto = "auto"

// MinCommandMemory is the smallest command memory cap that
// device.command-memory-max accepts.
const MinCommandMemory = 16 << 20

// CommandMemory parses device.command-memory-max. It returns auto true for
// auto, 0 for off or 0, and otherwise the cap in bytes.
func (d Device) CommandMemory() (n int64, auto bool, err error) {
	v := strings.TrimSpace(strings.ToLower(d.CommandMemoryMax))
	switch v {
	case CommandMemoryAuto:
		return 0, true, nil
	case "off", "0":
		return 0, false, nil
	}
	shift := 0
	num := strings.TrimSuffix(strings.TrimSuffix(v, "b"), "i")
	if num != "" {
		if i := strings.IndexByte("kmgt", num[len(num)-1]); i >= 0 {
			shift = 10 * (i + 1)
			num = num[:len(num)-1]
		}
	}
	n, perr := strconv.ParseInt(num, 10, 64)
	if perr != nil || n < 0 || n > math.MaxInt64>>shift || n<<shift < MinCommandMemory {
		return 0, false, fmt.Errorf("%w (at least 16M): %q", ErrMemorySize, d.CommandMemoryMax)
	}
	return n << shift, false, nil
}
