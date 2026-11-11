package web

import (
	"crypto/subtle"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const consentHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in Firefox - Hermes Hub</title><link rel="stylesheet" href="/static/app.css"></head>
<body class="doc"><main class="page">
<h1>Sign in Firefox to Hermes</h1>
{{if .Error}}<p class="err">{{.Error}}</p><p>You can close this window.</p>
{{else}}<p><b>{{.Req.Browser}}</b> on the computer <b>{{.Device.Name}}</b> asks to sign in to Hermes as <b>{{.User}}</b>. Hermes can then use this browser, with your logins, in its own tab group, as the device's settings and your approvals allow.</p>
<p>Only continue if you started this yourself from the Hermes extension in that browser just now.</p>
<dl class="facts">
<dt>Computer</dt><dd>{{.Device.Name}}</dd>
<dt>Host name</dt><dd>{{.Device.Hostname}}</dd>
<dt>Browser</dt><dd>{{.Req.Browser}}</dd>
<dt>Hermes account</dt><dd>{{.User}}</dd>
</dl>
<form method="post" action="/browser/authorize">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<input type="hidden" name="response_type" value="code">
<input type="hidden" name="client_id" value="{{.Req.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.Req.RedirectURI}}">
<input type="hidden" name="state" value="{{.Req.State}}">
<input type="hidden" name="code_challenge" value="{{.Req.Challenge}}">
<input type="hidden" name="code_challenge_method" value="S256">
<input type="hidden" name="device_id" value="{{.Req.DeviceID}}">
<input type="hidden" name="instance" value="{{.Req.Instance}}">
<input type="hidden" name="browser" value="{{.Req.Browser}}">
<div class="buttons"><button class="primary" type="submit" name="action" value="approve">Allow</button>
<button class="ghost" type="submit" name="action" value="deny">Do not allow</button></div>
</form>
{{end}}</main></body></html>`

func newConsentPage() *template.Template {
	return template.Must(template.New("consent").Parse(consentHTML))
}

var (
	challengeRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	instanceRE  = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)
	deviceIDRE  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

const maxState = 512

// authzRequest is a browser sign-in request from the Hermes Firefox
// extension, as the query of GET /browser/authorize or the consent form.
type authzRequest struct {
	ClientID, RedirectURI, State, Challenge, DeviceID, Instance, Browser string
}

type consentView struct {
	User, CSRF, Error string
	Req               authzRequest
	Device            hub.Device
}

func printableASCII(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// parseAuthz checks a sign-in request. The error is for the page; the hub
// never redirects a request it did not accept.
func parseAuthz(get func(string) string) (authzRequest, string) {
	q := authzRequest{ClientID: get("client_id"), RedirectURI: get("redirect_uri"), State: get("state"),
		Challenge: get("code_challenge"), DeviceID: get("device_id"), Instance: get("instance")}
	if q.ClientID != protocol.BrowserExtensionID || q.RedirectURI != protocol.BrowserRedirectURL(q.ClientID) {
		return q, "This sign-in request does not come from the Hermes Firefox extension."
	}
	if get("response_type") != "code" || get("code_challenge_method") != "S256" || !challengeRE.MatchString(q.Challenge) ||
		q.State == "" || len(q.State) > maxState || !printableASCII(q.State) || !deviceIDRE.MatchString(q.DeviceID) ||
		!instanceRE.MatchString(q.Instance) {
		return q, "This sign-in request is incomplete or malformed. Start the sign-in again from the Hermes extension."
	}
	name, ok := cleanDeviceName(get("browser"))
	if !ok {
		name = "Firefox"
	}
	q.Browser = name
	return q, ""
}

func (s *Server) renderConsent(w http.ResponseWriter, code int, v consentView) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	formAction := "'self'"
	if u, err := url.Parse(protocol.BrowserRedirectURL(protocol.BrowserExtensionID)); err == nil {
		formAction += " " + u.Scheme + "://" + u.Host
	}
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; "+
		"frame-ancestors 'none'; form-action "+formAction)
	w.WriteHeader(code)
	if err := s.consentPage.Execute(w, v); err != nil {
		s.log.Debug("render consent page", "error", err.Error())
	}
}

// consentDevice finds the device a request names and checks that user may
// sign a browser in for it. The message is for the page.
func (s *Server) consentDevice(r *http.Request, user, id string) (hub.Device, int, string) {
	d, ok, err := s.devices.Device(r.Context(), id)
	if err != nil {
		s.log.Error("look up device for browser sign-in", "error", err.Error())
		return hub.Device{}, http.StatusInternalServerError, msgSomethingWrong
	}
	if !ok || !d.Paired {
		return hub.Device{}, http.StatusNotFound, "This computer is not connected to this Hermes hub. Check the hub address in " +
			"the Hermes extension, or connect the computer with hermes-device login first."
	}
	if d.User != user {
		s.log.Warn("browser sign-in refused: device of another user", "user", user, "device", d.ID)
		return hub.Device{}, http.StatusForbidden, "This computer is connected to Hermes as another account. Sign in to " +
			"Hermes as that account to use Firefox on it."
	}
	return d, http.StatusOK, ""
}

// handleBrowserAuthorizePage shows the consent page of a browser sign-in.
// It changes nothing: Firefox loads the URL once without a window first.
func (s *Server) handleBrowserAuthorizePage(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		http.NotFound(w, r)
		return
	}
	req, msg := parseAuthz(r.URL.Query().Get)
	if msg != "" {
		s.renderConsent(w, http.StatusBadRequest, consentView{Error: msg})
		return
	}
	sess, ok := s.session(r)
	if !ok {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	d, code, msg := s.consentDevice(r, sess.User, req.DeviceID)
	if msg != "" {
		s.renderConsent(w, code, consentView{User: sess.User, Error: msg})
		return
	}
	s.renderConsent(w, http.StatusOK, consentView{User: sess.User, CSRF: sess.CSRF, Req: req, Device: d})
}

func redirectBack(w http.ResponseWriter, r *http.Request, req authzRequest, params url.Values) {
	params.Set("state", req.State)
	http.Redirect(w, r, req.RedirectURI+"?"+params.Encode(), http.StatusSeeOther)
}

func (s *Server) handleBrowserAuthorize(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		http.NotFound(w, r)
		return
	}
	sess, ok := s.session(r)
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	if !s.originOK(r) || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.CSRF)) != 1 {
		http.Error(w, "bad request origin or CSRF token", http.StatusForbidden)
		return
	}
	req, msg := parseAuthz(r.PostFormValue)
	if msg != "" {
		s.renderConsent(w, http.StatusBadRequest, consentView{User: sess.User, Error: msg})
		return
	}
	if r.PostFormValue("action") == "deny" {
		s.log.Info("browser sign-in declined", "user", sess.User, "device", req.DeviceID)
		redirectBack(w, r, req, url.Values{"error": {"access_denied"}})
		return
	}
	if r.PostFormValue("action") != "approve" {
		s.renderConsent(w, http.StatusBadRequest, consentView{User: sess.User, Error: "Choose Allow or Do not allow."})
		return
	}
	d, code, msg := s.consentDevice(r, sess.User, req.DeviceID)
	if msg != "" {
		s.renderConsent(w, code, consentView{User: sess.User, Error: msg})
		return
	}
	c, err := s.devices.CreateBrowserCode(r.Context(), hub.BrowserGrant{User: sess.User, DeviceID: d.ID, InstanceID: req.Instance,
		Browser: req.Browser, Challenge: req.Challenge, RedirectURI: req.RedirectURI})
	if err != nil {
		s.log.Error("create browser code", "error", err.Error())
		s.renderConsent(w, http.StatusInternalServerError, consentView{User: sess.User, Error: msgSomethingWrong})
		return
	}
	s.log.Info("browser sign-in approved", "user", sess.User, "device", d.ID, "browser", req.Browser)
	redirectBack(w, r, req, url.Values{"code": {c}})
}

type browserJSON struct {
	ID         string    `json:"id"`
	Browser    string    `json:"browser"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

// browsersByDevice lists user's browser sign-ins by device id.
func (s *Server) browsersByDevice(r *http.Request, user string) (map[string][]browserJSON, error) {
	bs, err := s.devices.BrowserSessions(r.Context(), user)
	if err != nil {
		return nil, err
	}
	out := map[string][]browserJSON{}
	for _, b := range bs {
		out[b.DeviceID] = append(out[b.DeviceID], browserJSON{ID: b.ID, Browser: b.Browser, CreatedAt: b.CreatedAt,
			LastUsedAt: b.LastUsedAt})
	}
	return out, nil
}

func (s *Server) handleRevokeBrowser(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		errorJSON(w, http.StatusNotFound, "no such browser")
		return
	}
	user, id := userFrom(r), r.PathValue("browser")
	deviceID, ok, err := s.devices.RevokeBrowserSession(r.Context(), user, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		errorJSON(w, http.StatusNotFound, "no such browser")
		return
	}
	s.log.Info("browser sign-in removed", "user", user, "device", deviceID, "browser_session", id)
	if s.onBrowserOut != nil {
		s.onBrowserOut(deviceID, id)
	}
	w.WriteHeader(http.StatusNoContent)
}
