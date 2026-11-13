package config

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Auth modes.
const (
	AuthLocal = "local"
	AuthProxy = "proxy"
	AuthOIDC  = "oidc"
)

// Auth configures how people log in.
type Auth struct {
	Mode           string   `name:"mode" description:"How people log in: local (accounts in the hub's database, with a setup page that makes the first one), proxy (a reverse proxy in front of the hub tells it who the user is) or oidc (an OpenID Connect provider). Default: oidc when oidc.issuer_url is set, else local"`
	Admins         []string `name:"admins" description:"Hub user names that are admins besides accounts marked admin in Settings > Users: admins manage people and connect models (Settings > Models; models.admins can limit that per backend). The first local account is an admin"`
	TrustedProxies []string `name:"trusted_proxies" description:"CIDRs or addresses of the reverse proxies in front of the hub, such as 10.0.0.0/8 or 127.0.0.1. In proxy mode the hub reads the user header only on requests from them. In every mode the hub takes the client address for login limits from their X-Forwarded-For"`
	Local          Local    `name:"local"`
	Proxy          Proxy    `name:"proxy"`
}

// Local configures accounts kept in the hub's database.
type Local struct {
	MaxFailures int           `name:"max_failures" default:"5" description:"Wrong passwords in a row that lock an account. 0 is the default"`
	Lockout     time.Duration `name:"lockout" default:"15m" description:"How long a locked account stays locked. An admin can unlock it sooner by setting a new password. 0 is the default"`
}

// Proxy configures login by a reverse proxy that tells the hub who the
// user is, such as Authelia, Authentik's proxy outpost, oauth2-proxy,
// tailscale serve, Cloudflare Access or an Envoy Gateway SecurityPolicy.
type Proxy struct {
	UserHeader   string `name:"user_header" default:"Remote-User" description:"Header that holds the user's identity: Remote-User (Authelia, Authentik), X-Auth-Request-User or X-Forwarded-User (oauth2-proxy), Tailscale-User-Login (tailscale serve). Not used with proxy.jwt.jwks_url"`
	NameHeader   string `name:"name_header" default:"Remote-Name" description:"Header with the user's display name. Not used with proxy.jwt.jwks_url"`
	EmailHeader  string `name:"email_header" default:"Remote-Email" description:"Header with the user's email address. Not used with proxy.jwt.jwks_url"`
	GroupsHeader string `name:"groups_header" default:"Remote-Groups" description:"Header with the user's groups, separated by commas or |, for backends[].groups. Not used with proxy.jwt.jwks_url"`
	AutoCreate   bool   `name:"auto_create" default:"false" description:"Make an account for each new identity the proxy sends. Off: only identities in users[] and accounts an admin made log in"`
	LogoutURL    string `name:"logout_url" description:"Where Log out sends the browser after the hub's own logout, such as the proxy's logout page. Empty: the hub's start page"`
	JWT          JWT    `name:"jwt"`
}

// JWT configures a signed token from the proxy, checked against the
// signer's keys, so the hub does not have to trust a plain header.
type JWT struct {
	JWKSURL     string   `name:"jwks_url" description:"URL of the JSON Web Key Set that signs the proxy's tokens, such as https://<team>.cloudflareaccess.com/cdn-cgi/access/certs or the jwks_uri of the identity provider. Set: the hub reads the user from a verified token instead of user_header"`
	Header      string   `name:"header" description:"Header that holds the token: Authorization (Bearer), Cf-Access-Jwt-Assertion (Cloudflare Access) or the header an Envoy Gateway SecurityPolicy forwards the ID token in. Default Authorization when cookie is empty"`
	Cookie      string   `name:"cookie" description:"Cookie that holds the token, such as CF_Authorization. Read when header has none"`
	Issuer      string   `name:"issuer" description:"Token issuer (iss) to require. Required with jwks_url"`
	Audiences   []string `name:"audiences" description:"Audiences (aud) the hub accepts: the OIDC client ID, or the application AUD tag of Cloudflare Access. Empty: any audience"`
	UserClaim   string   `name:"user_claim" default:"sub" description:"Claim that holds the user's identity, such as sub or email"`
	NameClaim   string   `name:"name_claim" default:"preferred_username" description:"Claim with the user's display name"`
	EmailClaim  string   `name:"email_claim" default:"email" description:"Claim with the user's email address"`
	GroupsClaim string   `name:"groups_claim" default:"groups" description:"Claim with the user's groups, for backends[].groups"`
}

// Validation errors of the auth settings.
var (
	ErrAuthMode = errors.New("auth.mode must be local, proxy or oidc")
	ErrOIDC     = errors.New("invalid oidc settings")
	ErrProxy    = errors.New("invalid auth.proxy settings")
	ErrLocal    = errors.New("invalid auth.local settings")
)

// Defaults of the local login limits, also for a zero value.
const (
	defaultMaxFailures = 5
	defaultLockout     = 15 * time.Minute
)

// defaultUserClaim is proxy.jwt.user_claim's default, also for "".
const defaultUserClaim = "sub"

// Proxies parses TrustedProxies.
func (a Auth) Proxies() ([]netip.Prefix, error) {
	out, err := parsePrefixes(a.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("%w: auth.trusted_proxies %w", ErrProxy, err)
	}
	return out, nil
}

func parsePrefixes(list []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return nil, fmt.Errorf("%q: %w", s, err)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func (c *Config) validateAuth() error {
	a := &c.Auth
	if a.Mode == "" {
		a.Mode = AuthLocal
		if c.OIDC.IssuerURL != "" {
			a.Mode = AuthOIDC
		}
	}
	if _, err := a.Proxies(); err != nil {
		return err
	}
	if a.Local.MaxFailures < 0 || a.Local.Lockout < 0 {
		return fmt.Errorf("%w: max_failures and lockout must not be negative", ErrLocal)
	}
	if a.Local.MaxFailures == 0 {
		a.Local.MaxFailures = defaultMaxFailures
	}
	if a.Local.Lockout == 0 {
		a.Local.Lockout = defaultLockout
	}
	switch a.Mode {
	case AuthLocal:
	case AuthProxy:
		return c.validateProxy()
	case AuthOIDC:
		return c.validateOIDC()
	default:
		return ErrAuthMode
	}
	return nil
}

func (c *Config) validateOIDC() error {
	o := &c.OIDC
	if o.IssuerURL == "" || o.ClientID == "" || o.ClientSecret == "" {
		return fmt.Errorf("%w: auth.mode oidc needs oidc.issuer_url, oidc.client_id and oidc.client_secret (or client_secret_file)", ErrOIDC)
	}
	if _, ok := webURL(o.IssuerURL); !ok {
		return fmt.Errorf("%w: oidc.issuer_url must be an http or https URL", ErrOIDC)
	}
	if o.AutoCreate && len(o.AllowedDomains) == 0 && len(o.AllowedGroups) == 0 {
		return fmt.Errorf("%w: oidc.auto_create needs allowed_domains or allowed_groups (allowed_domains: \"*\" for any verified email)", ErrOIDC)
	}
	return nil
}

func (c *Config) validateProxy() error {
	p := &c.Auth.Proxy
	j := &p.JWT
	if j.JWKSURL == "" {
		if len(c.Auth.TrustedProxies) == 0 {
			return fmt.Errorf("%w: auth.mode proxy needs auth.trusted_proxies, or proxy.jwt.jwks_url to verify a signed token", ErrProxy)
		}
		if strings.TrimSpace(p.UserHeader) == "" {
			return fmt.Errorf("%w: proxy.user_header is empty", ErrProxy)
		}
	} else {
		if _, ok := webURL(j.JWKSURL); !ok {
			return fmt.Errorf("%w: proxy.jwt.jwks_url must be an http or https URL", ErrProxy)
		}
		if j.Issuer == "" {
			return fmt.Errorf("%w: proxy.jwt.issuer is required with jwks_url", ErrProxy)
		}
		if j.Header == "" && j.Cookie == "" {
			j.Header = "Authorization"
		}
		if j.UserClaim == "" {
			j.UserClaim = defaultUserClaim
		}
	}
	if p.LogoutURL != "" {
		if _, ok := webURL(p.LogoutURL); !ok && !strings.HasPrefix(p.LogoutURL, "/") {
			return fmt.Errorf("%w: proxy.logout_url must be an http or https URL or a path", ErrProxy)
		}
	}
	return nil
}
