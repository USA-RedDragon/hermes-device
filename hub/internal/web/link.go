package web

import (
	"crypto/subtle"
	"net/http"
	"net/url"
)

const linkHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Link Telegram - Talaria</title><link rel="stylesheet" href="/static/app.css"></head>
<body class="doc"><main class="page">
<h1>Link Telegram</h1>
{{if .Done}}<p>Your Telegram account is now linked to <b>{{.User}}</b>. You can close this page and go back to the bot.</p>
{{else if .Error}}<p class="err">{{.Error}}</p><p>Send /link to the bot again to get a new link.</p>
{{else}}<p>Link the Telegram account that sent <code>/link</code> to the bot with the hub user <b>{{.User}}</b>?</p>
<p>That Telegram account can then use every Hermes backend that <b>{{.User}}</b> can use. Only continue if you sent <code>/link</code> yourself just now.</p>
<form method="post" action="/link/telegram">
<input type="hidden" name="token" value="{{.Token}}"><input type="hidden" name="csrf" value="{{.CSRF}}">
<button class="primary" type="submit">Link account</button></form>
{{end}}</main></body></html>`

const refusedHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Not set up - Talaria</title><link rel="stylesheet" href="/static/app.css"></head>
<body class="doc"><main class="page"><h1>Not set up</h1>
<p>You are signed in, but your account is not set up for Talaria. Ask the owner to add you.</p></main></body></html>`

// renderRefused answers a login whose sub is not in users. It sets no
// session.
func (s *Server) renderRefused(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(refusedHTML))
}

type linkView struct {
	User, Token, CSRF, Error string
	Done                     bool
}

func (s *Server) renderLink(w http.ResponseWriter, code int, v linkView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err := s.linkPage.Execute(w, v); err != nil {
		s.log.Debug("render link page", "error", err.Error())
	}
}

// handleLinkPage asks a logged-in user to confirm the link. Binding needs
// the POST, so a link opened by accident or planted by someone else cannot
// bind their Telegram account to this user.
func (s *Server) handleLinkPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	sess, ok := s.session(r)
	if !ok {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape("/link/telegram?token="+url.QueryEscape(token)), http.StatusFound)
		return
	}
	if token == "" {
		s.renderLink(w, http.StatusBadRequest, linkView{User: sess.User, Error: "This link has no token."})
		return
	}
	s.renderLink(w, http.StatusOK, linkView{User: sess.User, Token: token, CSRF: sess.CSRF})
}

func (s *Server) handleLink(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(r)
	if !ok {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	if !s.originOK(r) || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.CSRF)) != 1 {
		http.Error(w, "bad request origin or CSRF token", http.StatusForbidden)
		return
	}
	id, ok, err := s.tokens.ConsumeLinkToken(r.Context(), r.PostFormValue("token"))
	if err != nil {
		s.log.Error("consume link token", "error", err.Error())
		s.renderLink(w, http.StatusInternalServerError, linkView{User: sess.User, Error: "Something went wrong."})
		return
	}
	if !ok {
		s.renderLink(w, http.StatusBadRequest, linkView{User: sess.User, Error: "This link is used or expired."})
		return
	}
	if err := s.links.LinkTelegram(r.Context(), id, sess.User); err != nil {
		s.log.Error("link telegram", "error", err.Error())
		s.renderLink(w, http.StatusInternalServerError, linkView{User: sess.User, Error: "Something went wrong."})
		return
	}
	s.log.Info("linked telegram account", "user", sess.User, "telegram_id", id)
	s.renderLink(w, http.StatusOK, linkView{User: sess.User, Done: true})
}
