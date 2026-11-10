// Package config holds the hub's configuration.
package config

//go:generate go tool configulator -type Config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
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
	IssuerURL     string   `name:"issuer_url" required:"true" description:"OIDC issuer URL, such as https://auth.example.com/application/o/hermes-hub/"`
	ClientID      string   `name:"client_id" required:"true" description:"OIDC client ID"`
	ClientSecret  string   `name:"client_secret" required:"true" secret:"true" description:"OIDC client secret"`
	Scopes        []string `name:"scopes" default:"openid,profile,email" description:"Scopes to request. Authentik's profile scope carries preferred_username and groups"`
	UsernameClaim string   `name:"username_claim" default:"preferred_username" description:"ID token claim that holds the user name that backends[].users lists"`
	GroupsClaim   string   `name:"groups_claim" default:"groups" description:"ID token claim that holds the group names that backends[].groups lists"`
}

// Session configures the login cookie.
type Session struct {
	Secret string        `name:"secret" required:"true" secret:"true" description:"Key for the session and login cookies. At least 32 characters"`
	TTL    time.Duration `name:"ttl" default:"168h" description:"How long a login lasts"`
}

// Store configures the embedded database.
type Store struct {
	Path string `name:"path" default:"/data/hermes-hub.db" description:"Path of the SQLite database that holds Telegram links, link tokens and users' last seen groups"`
}

// Backend is one Hermes gateway Webapp listener.
type Backend struct {
	ID            string   `name:"id" description:"Stable backend ID: lowercase letters, digits and dashes"`
	Name          string   `name:"name" description:"Display name"`
	URL           string   `name:"url" description:"Base URL of the Hermes Webapp listener, such as http://desktop:9120"`
	Provider      string   `name:"provider" description:"Hermes dashboard password provider. Default basic"`
	Username      string   `name:"username" description:"Hermes dashboard user name"`
	Password      string   `name:"password" secret:"true" description:"Hermes dashboard password"`
	Users         []string `name:"users" description:"Hub user names that may use this backend"`
	Groups        []string `name:"groups" description:"Hub groups whose members may use this backend"`
	CWD           string   `name:"cwd" description:"Working directory for new sessions. Default: the gateway's own"`
	ShowReasoning *bool    `name:"show_reasoning" description:"Turn on reasoning streaming for each session the hub attaches to. Default true"`
}

// Config is the hub's configuration.
type Config struct {
	LogLevel string    `name:"log_level" default:"info" description:"Log level: debug, info, warn or error"`
	HTTP     HTTP      `name:"http"`
	OIDC     OIDC      `name:"oidc"`
	Session  Session   `name:"session"`
	Store    Store     `name:"store"`
	Backends []Backend `name:"backends" description:"Hermes backends and who may use them. Only settable in the config file"`
}

// Validation errors.
var (
	ErrInvalidLogLevel = errors.New("invalid log level")
	ErrShortSecret     = errors.New("session.secret must be at least 32 characters")
	ErrPublicURL       = errors.New("http.public_url must be an absolute http or https URL with no path")
	ErrBackend         = errors.New("invalid backend")
)

var backendIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Validate checks what tags can't and fills backend defaults.
func (c *Config) Validate() error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return ErrInvalidLogLevel
	}
	if c.Session.Secret != "" && len(c.Session.Secret) < 32 {
		return ErrShortSecret
	}
	if c.HTTP.PublicURL != "" {
		u, err := url.Parse(c.HTTP.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.Trim(u.Path, "/") != "" {
			return ErrPublicURL
		}
		c.HTTP.PublicURL = strings.TrimRight(c.HTTP.PublicURL, "/")
	}
	seen := map[string]bool{}
	for i := range c.Backends {
		b := &c.Backends[i]
		if !backendIDPattern.MatchString(b.ID) || seen[b.ID] {
			return fmt.Errorf("%w: backends[%d].id %q must be unique lowercase letters, digits and dashes", ErrBackend, i, b.ID)
		}
		seen[b.ID] = true
		u, err := url.Parse(b.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
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
	}
	return nil
}
