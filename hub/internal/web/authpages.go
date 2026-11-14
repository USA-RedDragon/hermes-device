package web

import (
	"html/template"
	"net/http"
	"net/netip"
	"net/url"

	"github.com/USA-RedDragon/talaria/hub/internal/auth"
	"github.com/USA-RedDragon/talaria/hub/internal/config"
)

// docsURL is the setup help the proxy error page links to.
// keyMode is the key of the login mode in the JSON the pages read.
const keyMode = "mode"

const docsURL = "https://usa-reddragon.github.io/talaria/hub/auth.html#behind-a-proxy-authmode-proxy"

// hostOf is the host of a URL, or "" when it has none.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}

// loginInfo is what Settings shows about how people log in: who may
// send identities and what happens to a new one. It holds no secret.
func loginInfo(cfg *config.Config, mode string, trusted []netip.Prefix) map[string]any {
	out := map[string]any{keyMode: mode, "issuer": hostOf(cfg.OIDC.IssuerURL)}
	cidrs := make([]string, 0, len(trusted))
	for _, p := range trusted {
		cidrs = append(cidrs, p.String())
	}
	switch mode {
	case config.AuthProxy:
		p := cfg.Auth.Proxy
		out["header"], out["jwt"], out["trusted"], out["auto_create"] = p.UserHeader, p.JWT.JWKSURL != "", cidrs, p.AutoCreate
	case config.AuthOIDC:
		out["auto_create"], out["domains"], out["groups"] = cfg.OIDC.AutoCreate, nonNil(cfg.OIDC.AllowedDomains), nonNil(cfg.OIDC.AllowedGroups)
	}
	return out
}

// serveAuthPage sends one of the static pages of the login and setup
// flow. They hold no data: their script asks /auth/info.
func serveAuthPage(w http.ResponseWriter, _ *http.Request, static *assets, file string) {
	page, _, err := static.page(file)
	if err != nil {
		http.Error(w, "missing page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

// handleLoginPage is the login page of auth.mode local. In the other
// modes the login starts at /auth/login; with no account yet the person
// has to set the hub up first.
func (s *Server) handleLoginPage(static *assets) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next := safeNext(r.URL.Query().Get("next"))
		switch {
		case s.mode != config.AuthLocal:
			http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(next), http.StatusFound)
		case s.needsSetup(r.Context()):
			http.Redirect(w, r, "/setup", http.StatusFound)
		default:
			if _, ok := s.session(r); ok {
				redirectNext(w, r, next)
				return
			}
			serveAuthPage(w, r, static, "login.html")
		}
	}
}

// handleSetupPage is the first-user page of auth.mode local. Once the hub
// has an account its script says so.
func (s *Server) handleSetupPage(static *assets) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.mode != config.AuthLocal {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		serveAuthPage(w, r, static, "setup.html")
	}
}

const docHead = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} - Talaria</title><link rel="icon" href="/static/favicon.svg" type="image/svg+xml"><link rel="stylesheet" href="/static/app.css"></head>
<body class="doc"><main class="page">
<div class="brand"><svg class="logo" viewBox="-0.1 -7.5 70 70" aria-hidden="true"><path d="M3.00 45.00 Q30.50 36.00 38.00 9.00 Q38.40 18.00 40.72 25.72 C47.80 18.30 57.30 12.60 66.80 12.60 C61.10 16.40 57.30 21.15 56.35 24.95 C59.20 23.05 62.05 24.47 64.42 24.95 C58.25 31.60 50.65 34.45 43.85 33.82 Q47.06 40.49 52.00 46.00 Q27.50 40.00 3.00 45.00 Z"/></svg>Talaria</div>
<h1>{{.Title}}</h1>
`

const refusedHTML = docHead + `<p>You are logged in as <code>{{.Who}}</code>, but this hub has no account for you.</p>
<p>Ask an admin of this hub to add you in Settings &gt; Users with this name or email.</p>
{{if .LogoutURL}}<p><a class="button" href="{{.LogoutURL}}">Log out</a></p>{{end}}
</main></body></html>`

const proxyErrorHTML = docHead + `<p>This hub lets the reverse proxy in front of it log people in, but this request came without a user.</p>
<p>If you set up this hub, check:</p>
<ol class="checks">
<li>The hub trusts your proxy's address. Add it to <code>auth.trusted_proxies</code> (the hub log shows the address it saw).</li>
<li>Your proxy sends the user in the header the hub reads, <code>auth.proxy.user_header</code> (for example Remote-User).</li>
<li>If you use signed tokens, the issuer, audience and keys in <code>auth.proxy.jwt</code> match your provider.</li>
</ol>
<p><a href="` + docsURL + `">Show setup help</a></p>
<p><a class="button" href="{{.Retry}}">Try again</a></p>
</main></body></html>`

type docView struct {
	Title, Who, Retry, LogoutURL string
}

func (s *Server) renderDoc(w http.ResponseWriter, code int, t *template.Template, v docView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := t.Execute(w, v); err != nil {
		s.log.Debug("render page", "error", err.Error())
	}
}

// renderRefused answers a login whose identity has no account, or whose
// account is disabled, and names the identity so an admin can add it
// without reading logs. It sets no session.
func (s *Server) renderRefused(w http.ResponseWriter, _ *http.Request, id auth.Identity) {
	who := id.Email
	if who == "" {
		who = id.ID
	}
	s.renderDoc(w, http.StatusForbidden, s.refusedPage, docView{Title: "Not set up", Who: who, LogoutURL: s.logoutURL})
}

// renderProxyError answers a request in auth.mode proxy that came with no
// valid identity, with what to check.
func (s *Server) renderProxyError(w http.ResponseWriter, r *http.Request) {
	s.renderDoc(w, http.StatusUnauthorized, s.proxyPage, docView{Title: "The proxy did not say who you are",
		Retry: "/auth/login?next=" + url.QueryEscape(safeNext(r.URL.Query().Get("next")))})
}
