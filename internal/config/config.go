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
)

// Hub is where the device connects.
type Hub struct {
	URL string `name:"url" description:"Base URL of hermes-hub, for example https://hub.example.com. Needed by login, run, chat and acp"`
}

// Store is where the device keeps its token and daemon state.
type Store struct {
	Keyring   bool   `name:"keyring" default:"true" description:"Keep the device token in the OS keyring when one is available"`
	TokenFile string `name:"token-file" description:"File (mode 0600) for the device token when the keyring is off or unavailable. Defaults to token in the state directory"`
	StateDir  string `name:"state-dir" description:"Directory for the token file and the daemon's state file. Defaults to hermes-device in the user config directory"`
}

// Device is what the run daemon lets the agent use on this device.
type Device struct {
	Roots   []string `name:"roots" description:"Absolute directories commands may run in. A command whose working directory is outside all of them is refused. Needed by run"`
	TempDir string   `name:"tmp-dir" description:"Directory Hermes uses on this device for its shell snapshot files. Defaults to the system temp directory"`
	Shell   string   `name:"shell" default:"bash" description:"Shell used to run commands"`
}

// Gateway is the run daemon's loopback splice that chat points the local
// Hermes TUI at.
type Gateway struct {
	Listen string `name:"listen" default:"127.0.0.1:0" description:"Loopback address of the chat splice. Port 0 picks a free port"`
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
