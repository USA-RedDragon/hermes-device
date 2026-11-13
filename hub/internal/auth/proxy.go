package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/coreos/go-oidc/v3/oidc"
)

// Identity is who a login says the user is.
type Identity struct {
	// ID is the identity the provider or proxy vouches for: an OIDC sub,
	// or the proxy's user header or token claim.
	ID            string
	Display       string
	Email         string
	EmailVerified bool
	Groups        []string
}

// Proxy errors.
var (
	ErrUntrustedPeer = errors.New("auth: request is not from a trusted proxy")
	ErrNoIdentity    = errors.New("auth: the proxy sent no identity")
	ErrBadToken      = errors.New("auth: the proxy's token is not valid")
)

// Proxy reads the identity a reverse proxy sends with each request.
type Proxy struct {
	cfg      config.Proxy
	trusted  []netip.Prefix
	verifier *oidc.IDTokenVerifier
}

// NewProxy returns the reader of proxy identities for cfg. httpClient
// fetches the JWKS; nil is http.DefaultClient.
func NewProxy(cfg config.Auth, httpClient *http.Client) (*Proxy, error) {
	trusted, err := cfg.Proxies()
	if err != nil {
		return nil, err
	}
	p := &Proxy{cfg: cfg.Proxy, trusted: trusted}
	if j := cfg.Proxy.JWT; j.JWKSURL != "" {
		ctx := context.Background()
		if httpClient != nil {
			ctx = oidc.ClientContext(ctx, httpClient)
		}
		keys := oidc.NewRemoteKeySet(ctx, j.JWKSURL)
		p.verifier = oidc.NewVerifier(j.Issuer, keys, &oidc.Config{SkipClientIDCheck: true,
			SupportedSigningAlgs: []string{oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.ES512, oidc.PS256, oidc.PS384, oidc.PS512, oidc.EdDSA}})
	}
	return p, nil
}

// Identity returns the identity the proxy sent with r. Without a JWKS URL
// it reads plain headers, and only from a trusted proxy. With one it reads
// the user from the signed token; trusted_proxies, when set, still limit
// who may send it.
func (p *Proxy) Identity(r *http.Request) (Identity, error) {
	if (p.verifier == nil || len(p.trusted) > 0) && !Trusted(Peer(r), p.trusted) {
		return Identity{}, ErrUntrustedPeer
	}
	if p.verifier != nil {
		return p.token(r)
	}
	h := r.Header
	id := Identity{ID: strings.TrimSpace(h.Get(p.cfg.UserHeader))}
	if id.ID == "" {
		return Identity{}, ErrNoIdentity
	}
	if p.cfg.NameHeader != "" {
		id.Display = strings.TrimSpace(h.Get(p.cfg.NameHeader))
	}
	if p.cfg.EmailHeader != "" {
		id.Email = strings.TrimSpace(h.Get(p.cfg.EmailHeader))
		id.EmailVerified = id.Email != ""
	}
	if p.cfg.GroupsHeader != "" {
		id.Groups = splitGroups(h.Values(p.cfg.GroupsHeader))
	}
	return id, nil
}

func splitGroups(values []string) []string {
	var out []string
	for _, v := range values {
		for g := range strings.FieldsFuncSeq(v, func(r rune) bool { return r == ',' || r == '|' }) {
			if g = strings.TrimSpace(g); g != "" && !slices.Contains(out, g) {
				out = append(out, g)
			}
		}
	}
	return out
}

func (p *Proxy) raw(r *http.Request) string {
	j := p.cfg.JWT
	if j.Header != "" {
		v := strings.TrimSpace(r.Header.Get(j.Header))
		if scheme, tok, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "bearer") {
			v = strings.TrimSpace(tok)
		}
		if v != "" {
			return v
		}
	}
	if j.Cookie != "" {
		if c, err := r.Cookie(j.Cookie); err == nil {
			return c.Value
		}
	}
	return ""
}

func (p *Proxy) token(r *http.Request) (Identity, error) {
	raw := p.raw(r)
	if raw == "" {
		return Identity{}, ErrNoIdentity
	}
	tok, err := p.verifier.Verify(r.Context(), raw)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrBadToken, err)
	}
	j := p.cfg.JWT
	if len(j.Audiences) > 0 && !slices.ContainsFunc(tok.Audience, func(a string) bool { return slices.Contains(j.Audiences, a) }) {
		return Identity{}, fmt.Errorf("%w: audience %v is not accepted", ErrBadToken, tok.Audience)
	}
	cl := Claims{}
	if err := tok.Claims(&cl); err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrBadToken, err)
	}
	id := Identity{ID: cl.Str(j.UserClaim), Display: cl.Str(j.NameClaim), Email: cl.Str(j.EmailClaim), Groups: cl.List(j.GroupsClaim)}
	if id.ID == "" {
		return Identity{}, fmt.Errorf("%w: no %s claim", ErrBadToken, j.UserClaim)
	}
	verified, present := cl["email_verified"].(bool)
	id.EmailVerified = id.Email != "" && (verified || !present)
	return id, nil
}

// Claims are a token's claims.
type Claims map[string]any

// Str is a string claim, or "".
func (c Claims) Str(name string) string {
	s, _ := c[name].(string)
	return s
}

// List is a claim that holds one string or a list of strings.
func (c Claims) List(name string) []string {
	switch v := c[name].(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{v}
	}
	return nil
}
