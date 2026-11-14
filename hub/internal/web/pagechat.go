package web

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// pageChatCSP is the hub's policy with one change: an extension page may
// frame the page chat view. Firefox gives the frame of an extension that
// has host permission for the hub the hub's first-party cookies, so the
// user's session works there (talaria docs/browser.md section 6.2).
const pageChatCSP = "default-src 'self'; img-src 'self' data: blob:; object-src 'none'; base-uri 'none'; " +
	"frame-ancestors 'self' moz-extension:; form-action 'self'"

// handlePageChatView serves the hub page in page chat mode, for the frame
// in the Firefox extension's toolbar popup and sidebar. It is the hub's
// own page and scripts; the class on <html> switches app.js to the page
// chat layout.
func (s *Server) handlePageChatView(static *assets) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		page, _, err := static.page("index.html")
		if err != nil {
			http.Error(w, "missing page", http.StatusInternalServerError)
			return
		}
		page = bytes.Replace(page, []byte(`<html lang="en">`), []byte(`<html lang="en" class="pagechat">`), 1)
		h := w.Header()
		h.Set("Content-Security-Policy", pageChatCSP)
		h.Del("X-Frame-Options")
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Length", strconv.Itoa(len(page)))
		_, _ = w.Write(page)
	}
}

// handleNewPageChat starts a page chat: a chat in the sandbox of the
// backend of the computer that runs the extension (else the user's default
// backend) whose browser device is that computer, marked as a page chat.
func (s *Server) handleNewPageChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BackendID string `json:"backend_id"`
		DeviceID  string `json:"device_id"`
	}
	if !decode(w, r, &body) {
		return
	}
	user := userFrom(r)
	backendID := body.BackendID
	if body.DeviceID != "" && s.computers != nil {
		cs, err := s.computers.Computers(r.Context(), user)
		if err != nil {
			s.fail(w, err)
			return
		}
		found := false
		for _, c := range cs {
			if c.ID == body.DeviceID {
				backendID, found = c.BackendID, true
			}
		}
		if !found {
			errorJSON(w, http.StatusNotFound, "No such computer. It may have been removed")
			return
		}
	}
	sess, err := s.svc.NewSession(r.Context(), user, backendID)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := struct {
		sessionJSON
		BrowserDevice string `json:"browser_device,omitempty"`
		BrowserError  string `json:"browser_error,omitempty"`
	}{sessionJSON: toJSON(sess)}
	out.Page = true
	if s.chats != nil {
		if err := s.chats.MarkPage(r.Context(), user, sess.BackendID, sess.ID); err != nil {
			s.log.Warn("mark page chat", "error", err.Error())
		}
	}
	if body.DeviceID != "" && s.computers != nil {
		err := s.computers.UseBrowser(r.Context(), user, sess.BackendID, sess.ID, body.DeviceID)
		if err == nil {
			out.BrowserDevice = body.DeviceID
		} else {
			s.log.Warn("page chat browser device", "error", err.Error())
			out.BrowserError = browserReason(err)
		}
	}
	writeJSON(w, http.StatusCreated, out)
}

// browserReason says why a page chat runs without its computer's browser.
func browserReason(err error) string {
	switch {
	case errors.Is(err, hub.ErrComputerOffline):
		return "This computer's Firefox is not connected to Talaria, so Talaria cannot read its tabs yet"
	case errors.Is(err, hub.ErrUnknownDevice):
		return "The hub does not know this computer"
	default:
		return "The agent did not set this computer's browser for the chat: the server runs an older Talaria. Upgrade Talaria on the server: https://usa-reddragon.github.io/talaria/deploy/upgrades.html"
	}
}
