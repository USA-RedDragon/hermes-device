package web

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// Wrong pairing codes a user may type within codeFailureWindow.
const (
	maxCodeFailures   = 10
	codeFailureWindow = 15 * time.Minute
	maxDeviceName     = 64
)

const pairHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Connect a computer - Talaria</title><link rel="stylesheet" href="/static/app.css"></head>
<body class="doc"><main class="page">
<h1>Connect a computer</h1>
{{if .Done}}<p><b>{{.Name}}</b> is now connected to {{.BackendName}}. Go back to the computer: it finishes on its own in a few seconds.</p>
<p>You can remove it at any time under Settings on the <a href="/">chat page</a>.</p>
{{else if .Declined}}<p>The computer was not connected. You can close this page.</p>
{{else if .Pairing}}<p>A computer asks to connect to {{if .BackendName}}{{.BackendName}}{{else}}your assistant{{end}} as <b>{{.User}}</b>. {{if .BackendName}}It{{else}}The one you pick below{{end}} can then run commands and change files on it, in the folders it allows.</p>
<p>Only continue if you started this yourself on that computer just now, and the details below match it.</p>
<dl class="facts">
<dt>Computer</dt><dd>{{.Pairing.Hostname}}</dd>
<dt>System</dt><dd>{{.Pairing.OS}} {{.Pairing.Arch}}</dd>
<dt>Address</dt><dd>{{.Pairing.Addr}}</dd>
<dt>Asked at</dt><dd>{{.Asked}}</dd>
<dt>talaria</dt><dd>{{.Pairing.ClientVersion}}</dd>
</dl>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" action="/device/pair">
<input type="hidden" name="code" value="{{.Pairing.UserCode}}"><input type="hidden" name="csrf" value="{{.CSRF}}">
<label>Name for this computer <input name="name" value="{{.Name}}" maxlength="64" required autocomplete="off"></label>
{{if gt (len .Backends) 1}}<label>Hermes <select name="backend">{{range .Backends}}<option value="{{.ID}}"{{if eq .ID $.Backend}} selected{{end}}>{{.Name}}</option>{{end}}</select></label>
{{else}}{{range .Backends}}<input type="hidden" name="backend" value="{{.ID}}">{{end}}{{end}}
<div class="buttons"><button class="primary" type="submit" name="action" value="approve">Connect this computer</button>
<button class="ghost" type="submit" name="action" value="deny">Do not connect</button></div>
</form>
{{else}}<p>Type the code that <code>talaria setup</code> shows on the computer you want to connect.</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if not .Locked}}<form method="post" action="/device/pair">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<label>Code <input name="code" autocomplete="off" autocapitalize="characters" spellcheck="false" placeholder="ABCD-EFGH" required autofocus></label>
<div class="buttons"><button class="primary" type="submit" name="action" value="lookup">Continue</button></div>
</form>{{end}}
{{end}}</main></body></html>`

type pairView struct {
	User, CSRF, Error, Name, Backend, BackendName, Asked string
	Pairing                                              *hub.Pairing
	Backends                                             []hub.Backend
	Done, Declined, Locked                               bool
}

func (s *Server) renderPair(w http.ResponseWriter, code int, v pairView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := s.pairPage.Execute(w, v); err != nil {
		s.log.Debug("render pair page", "error", err.Error())
	}
}

func newPairPage() *template.Template {
	return template.Must(template.New("pair").Parse(pairHTML))
}

// deviceBackends are the backends user may pair a device with.
func (s *Server) deviceBackends(r *http.Request, user string) ([]hub.Backend, error) {
	bs, err := s.svc.Backends(r.Context(), user)
	if err != nil {
		return nil, err
	}
	out := []hub.Backend{}
	for _, b := range bs {
		if s.deviceBackendIDs[b.ID] {
			out = append(out, b)
		}
	}
	return out, nil
}

// handlePairPage asks a signed-in user for the code a device shows. The
// code never comes from the link, so a link someone else sends cannot
// pair their computer with this user.
func (s *Server) handlePairPage(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		http.NotFound(w, r)
		return
	}
	sess, ok := s.session(r)
	if !ok {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape("/device/pair"), http.StatusFound)
		return
	}
	s.renderPair(w, http.StatusOK, pairView{User: sess.User, CSRF: sess.CSRF})
}

func cleanDeviceName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxDeviceName || !utf8.ValidString(name) {
		return name, false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return name, false
		}
	}
	return name, true
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
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
	v := pairView{User: sess.User, CSRF: sess.CSRF}
	ctx := r.Context()
	fails, err := s.devices.CodeFailures(ctx, sess.User, codeFailureWindow)
	if err != nil {
		s.pairFailed(w, v, "count pairing code failures", err)
		return
	}
	if fails >= maxCodeFailures {
		v.Error, v.Locked = "Too many wrong codes. Wait 15 minutes, then try again.", true
		s.renderPair(w, http.StatusTooManyRequests, v)
		return
	}
	p, ok, err := s.devices.PendingPairing(ctx, r.PostFormValue("code"))
	if err != nil {
		s.pairFailed(w, v, "look up pairing", err)
		return
	}
	if !ok {
		if _, err := s.devices.CodeFailure(ctx, sess.User, codeFailureWindow); err != nil {
			s.log.Error("record pairing code failure", "error", err.Error())
		}
		v.Error = "That code is wrong or has expired. Check it and try again, or run talaria setup again for a new code."
		s.renderPair(w, http.StatusBadRequest, v)
		return
	}
	backends, err := s.deviceBackends(r, sess.User)
	if err != nil {
		s.pairFailed(w, v, "list backends", err)
		return
	}
	if len(backends) == 0 {
		v.Error = "None of your Hermes assistants is set up for computers yet. Ask the owner to turn this on."
		s.renderPair(w, http.StatusForbidden, v)
		return
	}
	v.Pairing, v.Backends, v.Name = &p, backends, p.Name
	if len(backends) == 1 {
		v.BackendName = backends[0].Name
	}
	v.Asked = p.CreatedAt.UTC().Format("2006-01-02 15:04 UTC")
	v.Backend = s.defaults[sess.User]
	switch r.PostFormValue("action") {
	case "deny":
		if err := s.devices.DenyPairing(ctx, p.UserCode); err != nil {
			s.pairFailed(w, v, "deny pairing", err)
			return
		}
		s.log.Info("pairing declined", "user", sess.User, "hostname", p.Hostname)
		s.renderPair(w, http.StatusOK, pairView{User: sess.User, Declined: true})
	case "approve":
		s.approvePairing(w, r, sess.User, v)
	default:
		s.renderPair(w, http.StatusOK, v)
	}
}

func (s *Server) approvePairing(w http.ResponseWriter, r *http.Request, user string, v pairView) {
	name, ok := cleanDeviceName(r.PostFormValue("name"))
	v.Name = name
	if !ok {
		v.Error = "Give the computer a name of 1 to 64 characters."
		s.renderPair(w, http.StatusBadRequest, v)
		return
	}
	backendID := r.PostFormValue("backend")
	var picked *hub.Backend
	for i := range v.Backends {
		if v.Backends[i].ID == backendID {
			picked = &v.Backends[i]
		}
	}
	if picked == nil {
		v.Error = "Pick which Hermes this computer works with."
		s.renderPair(w, http.StatusBadRequest, v)
		return
	}
	v.Backend = picked.ID
	d, err := s.devices.ApprovePairing(r.Context(), v.Pairing.UserCode, user, picked.ID, name)
	switch {
	case errors.Is(err, hub.ErrNameTaken):
		v.Error = "You already have a computer with that name. Pick another name."
		s.renderPair(w, http.StatusConflict, v)
		return
	case errors.Is(err, hub.ErrUnknownPairing):
		s.renderPair(w, http.StatusBadRequest, pairView{User: user, CSRF: v.CSRF,
			Error: "This code has expired. Run talaria setup again for a new code."})
		return
	case err != nil:
		s.pairFailed(w, v, "approve pairing", err)
		return
	}
	s.log.Info("pairing approved", "user", user, "device", d.ID, "backend", d.BackendID, "name", d.Name)
	s.renderPair(w, http.StatusOK, pairView{User: user, Done: true, Name: d.Name, BackendName: picked.Name})
}

func (s *Server) pairFailed(w http.ResponseWriter, v pairView, what string, err error) {
	s.log.Error(what, "error", err.Error())
	v.Error = "Something went wrong. Try again."
	s.renderPair(w, http.StatusInternalServerError, v)
}

// Device kinds in the device list.
const (
	kindPaired  = "paired"
	kindSandbox = "sandbox"
)

type deviceJSON struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	Online        *bool      `json:"online,omitempty"`
	Name          string     `json:"name"`
	BackendID     string     `json:"backend_id"`
	BackendName   string     `json:"backend_name"`
	Hostname      string     `json:"hostname"`
	OS            string     `json:"os"`
	Arch          string     `json:"arch"`
	ClientVersion string     `json:"client_version,omitempty"`
	CreatedAt     time.Time  `json:"created_at,omitzero"`
	Paired        bool       `json:"paired"`
	LastSeenAt    *time.Time `json:"last_seen_at"`
	LastAddr      string     `json:"last_addr"`
	PrevAddr      string     `json:"prev_addr,omitempty"`
	AddrChangedAt *time.Time `json:"addr_changed_at,omitempty"`
	LastClose     *closeJSON `json:"last_close,omitempty"`
}

// closeJSON is how a computer's latest connection to its Hermes ended,
// shown while it has not connected again.
type closeJSON struct {
	Code   int       `json:"code"`
	Reason string    `json:"reason"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		writeJSON(w, http.StatusOK, []deviceJSON{})
		return
	}
	user := userFrom(r)
	ds, err := s.devices.Devices(r.Context(), user)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]deviceJSON, 0, len(ds))
	for _, d := range ds {
		j := deviceJSON{ID: d.ID, Kind: kindPaired, Name: d.Name, BackendID: d.BackendID, BackendName: s.backendNames[d.BackendID],
			Hostname: d.Hostname, OS: d.OS, Arch: d.Arch, ClientVersion: d.ClientVersion, CreatedAt: d.CreatedAt, Paired: d.Paired,
			LastSeenAt: optTime(d.LastSeenAt), LastAddr: d.LastAddr}
		if !d.AddrChangedAt.IsZero() {
			j.PrevAddr, j.AddrChangedAt = d.PrevAddr, optTime(d.AddrChangedAt)
		}
		if c := d.LastClose; !c.At.IsZero() {
			j.LastClose = &closeJSON{Code: c.Code, Reason: c.Reason, By: c.By, At: c.At}
		}
		if j.BackendName == "" {
			j.BackendName = d.BackendID
		}
		out = append(out, j)
	}
	if s.sandboxes != nil {
		for _, sb := range s.sandboxes(r.Context(), user) {
			name := s.backendNames[sb.BackendID]
			if name == "" {
				name = sb.BackendID
			}
			out = append(out, deviceJSON{ID: sb.ID, Kind: kindSandbox, Name: sb.Name, BackendID: sb.BackendID,
				BackendName: name, Paired: true, Online: sb.Online})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		errorJSON(w, http.StatusNotFound, "no such computer")
		return
	}
	user, id := userFrom(r), r.PathValue("device")
	ok, err := s.devices.RevokeDevice(r.Context(), user, id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		errorJSON(w, http.StatusNotFound, "no such computer")
		return
	}
	s.log.Info("device revoked", "user", user, "device", id)
	if s.onRevoke != nil {
		s.onRevoke(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRenameDevice(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		errorJSON(w, http.StatusNotFound, "no such computer")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	name, ok := cleanDeviceName(body.Name)
	if !ok {
		errorJSON(w, http.StatusBadRequest, fmt.Sprintf("Give the computer a name of 1 to %d characters", maxDeviceName))
		return
	}
	switch err := s.devices.RenameDevice(r.Context(), userFrom(r), r.PathValue("device"), name); {
	case errors.Is(err, hub.ErrNameTaken):
		errorJSON(w, http.StatusConflict, "You already have a computer with that name")
	case errors.Is(err, hub.ErrUnknownDevice):
		errorJSON(w, http.StatusNotFound, "no such computer")
	case err != nil:
		s.fail(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleDismissAddress(w http.ResponseWriter, r *http.Request) {
	if s.devices == nil {
		errorJSON(w, http.StatusNotFound, "no such computer")
		return
	}
	if err := s.devices.DismissAddressChange(r.Context(), userFrom(r), r.PathValue("device")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
