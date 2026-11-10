// Package config holds the device client's configuration.
package config

//go:generate go tool configulator -type Config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"
)

// Server is where the client dials.
type Server struct {
	URL string `name:"url" required:"true" description:"WebSocket URL of the Hermes device plugin, for example ws://127.0.0.1:9119/api/plugins/device/ws"`
}

// Device identifies this device to the server.
type Device struct {
	Name  string `name:"name" required:"true" description:"Device name, as used in /device <name> <dir>"`
	Token string `name:"token" required:"true" secret:"true" description:"Shared token for this device, matching the server's plugin config"`
}

// Reconnect controls the backoff between connection attempts.
type Reconnect struct {
	MinBackoff time.Duration `name:"min-backoff" default:"1s" description:"First delay after a lost connection"`
	MaxBackoff time.Duration `name:"max-backoff" default:"30s" description:"Longest delay between attempts"`
}

// Config is the device client's configuration.
type Config struct {
	LogLevel  string    `name:"log-level" default:"info" description:"Log level: debug, info, warn or error"`
	Server    Server    `name:"server"`
	Device    Device    `name:"device"`
	Roots     []string  `name:"roots" required:"true" description:"Absolute directories commands may run in. A command whose working directory is outside all of them is refused"`
	TempDir   string    `name:"tmp-dir" description:"Directory Hermes uses on this device for its shell snapshot files. Defaults to the system temp directory"`
	Shell     string    `name:"shell" default:"bash" description:"Shell used to run commands"`
	Reconnect Reconnect `name:"reconnect"`
}

// Validation errors.
var (
	ErrInvalidLogLevel = errors.New("invalid log level")
	ErrInvalidURL      = errors.New("server.url must be a ws:// or wss:// URL")
	ErrRelativePath    = errors.New("must be an absolute path")
	ErrBackoff         = errors.New("reconnect.min-backoff must be positive and not above reconnect.max-backoff")
)

// Validate checks what the tags can't.
func (c Config) Validate() error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return ErrInvalidLogLevel
	}
	u, err := url.Parse(c.Server.URL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return ErrInvalidURL
	}
	for i, r := range c.Roots {
		if !filepath.IsAbs(r) {
			return fmt.Errorf("roots[%d] %q: %w", i, r, ErrRelativePath)
		}
	}
	if c.TempDir != "" && !filepath.IsAbs(c.TempDir) {
		return fmt.Errorf("tmp-dir %q: %w", c.TempDir, ErrRelativePath)
	}
	if c.Reconnect.MinBackoff <= 0 || c.Reconnect.MinBackoff > c.Reconnect.MaxBackoff {
		return ErrBackoff
	}
	return nil
}
