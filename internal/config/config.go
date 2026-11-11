// Package config holds the configuration of every hermes-device subcommand.
package config

//go:generate go tool configulator -type Config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/mcp"
)

// Hub is where the device connects.
type Hub struct {
	URL       string `name:"url" description:"Base URL of hermes-hub, for example https://hub.example.com. Needed by login, run, chat and acp"`
	Token     string `name:"token" secret:"true" description:"Device token for run, instead of the one login stored, for a service device such as the sandbox pod. Wins over hub.token-file and the token store"`
	TokenFile string `name:"token-file" description:"File holding only the device token for run, for example a mounted Kubernetes secret. Must not be readable by others. Wins over the token store"`
}

// Store is where the device keeps its token and daemon state.
type Store struct {
	Keyring   bool   `name:"keyring" default:"true" description:"Keep the device token in the OS keyring when one is available"`
	TokenFile string `name:"token-file" description:"File (mode 0600) for the device token when the keyring is off or unavailable. Defaults to token in the state directory"`
	StateDir  string `name:"state-dir" description:"Directory for the token file and the daemon's state file. Defaults to hermes-device in the user config directory"`
}

// Device is what the run daemon lets the agent use on this device.
type Device struct {
	Roots        []string      `name:"roots" description:"Absolute directories commands may run in. A command whose working directory is outside all of them is refused. Needed by run"`
	TempDir      string        `name:"tmp-dir" description:"Directory Hermes uses on this device for its shell snapshot files. Defaults to the system temp directory"`
	Shell        string        `name:"shell" default:"bash" description:"Shell used to run commands"`
	ResumeBuffer int           `name:"resume-buffer" default:"1048576" description:"Most output bytes per command that run keeps for a resume of the exec leg until the plugin confirms them. Older output of a command is dropped, and the plugin shows the gap"`
	MaxGrace     time.Duration `name:"max-grace" default:"10m" description:"Longest time run keeps commands running after the exec leg drops, waiting for a resume. The plugin's grace_seconds applies when it is shorter"`
}

// Gateway is the run daemon's loopback splice that chat points the local
// Hermes TUI at.
type Gateway struct {
	Listen string `name:"listen" default:"127.0.0.1:0" description:"Loopback address of the chat splice. Port 0 picks a free port"`
	Splice bool   `name:"splice" default:"true" description:"Run the chat splice in run. false (or run --no-splice) runs only the exec leg: no listener, no daemon state file and no whoami call, for a headless service device such as the sandbox pod"`
}

// Chat runs the local Hermes TUI.
type Chat struct {
	Hermes string `name:"hermes" default:"hermes" description:"Local Hermes launcher, the same version as the server's Hermes"`
}

// ACPHost is the in-pod ACP process host.
type ACPHost struct {
	Listen           string `name:"listen" default:":9130" description:"Address the ACP host listens on. Only hermes-hub may reach it"`
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
	Enabled    bool     `name:"enabled" default:"false" description:"Serve the Hermes Firefox extension on a private socket in the state directory and offer browser control to the plugin, so the agent can use tabs in its tab group. Run hermes-device browser install once"`
	AllowHosts []string `name:"allow-hosts" description:"Loopback or private network host names the agent may open. All other loopback, private, link-local and CGNAT addresses are blocked in agent tabs"`
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
}

// Reconnect controls the backoff between connection attempts.
type Reconnect struct {
	MinBackoff time.Duration `name:"min-backoff" default:"1s" description:"First delay after a lost connection"`
	MaxBackoff time.Duration `name:"max-backoff" default:"30s" description:"Longest delay between attempts"`
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
	} {
		if p != "" && !filepath.IsAbs(p) {
			return fmt.Errorf("%s %q: %w", name, p, ErrRelativePath)
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
	if len(c.Device.Roots) == 0 {
		return fmt.Errorf("%w: device.roots", ErrMissing)
	}
	return nil
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
		}
		for k := range srv.Env {
			if k == "" || strings.ContainsAny(k, "=\x00") {
				return fmt.Errorf("%w: mcp.servers.%s.env has an invalid name %q", ErrMCPServer, name, k)
			}
		}
	}
	return nil
}
