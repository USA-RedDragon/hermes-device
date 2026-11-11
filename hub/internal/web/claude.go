package web

import (
	"errors"
	"net/http"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// claudeAdmin is the optional part of a hub.Claude that says whether a
// user signs Claude accounts in on any backend.
type claudeAdmin interface {
	Admin(user string) bool
}

// claudeRoutes adds the Claude accounts API. The sign-in routes answer
// only the backend's Claude admins; like every route but GET, they need
// the CSRF token and a same-origin request.
func (s *Server) claudeRoutes(mux *http.ServeMux) {
	const base = "/api/claude/{backend}/accounts"
	mux.Handle("GET "+base, s.api(s.handleClaudeAccounts))
	mux.Handle("GET "+base+"/{account}/usage", s.api(s.handleClaudeUsage))
	mux.Handle("POST "+base+"/{account}/signin", s.api(s.handleClaudeSignIn))
	mux.Handle("GET "+base+"/{account}/signin", s.api(s.handleClaudeSignIn))
	mux.Handle("DELETE "+base+"/{account}/signin", s.api(s.handleClaudeSignIn))
	mux.Handle("POST "+base+"/{account}/signin/code", s.api(s.handleClaudeCode))
	mux.Handle("GET /api/claude/{backend}/pool", s.api(s.handleClaudePool))
}

func (s *Server) isClaudeAdmin(user string) bool {
	a, ok := s.claude.(claudeAdmin)
	return ok && a.Admin(user)
}

// claudeFail answers a hub.Claude error. Its messages never hold the
// code or the sign-in URL.
func (s *Server) claudeFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hub.ErrNoClaude):
		errorJSON(w, http.StatusNotFound, hub.ErrNoClaude.Error())
	case errors.Is(err, hub.ErrUnknownAccount):
		errorJSON(w, http.StatusNotFound, hub.ErrUnknownAccount.Error())
	case errors.Is(err, hub.ErrNoSignIn):
		errorJSON(w, http.StatusNotFound, hub.ErrNoSignIn.Error())
	case errors.Is(err, hub.ErrWrongSignIn):
		errorJSON(w, http.StatusConflict, hub.ErrWrongSignIn.Error())
	case errors.Is(err, hub.ErrBadCode):
		errorJSON(w, http.StatusBadRequest, hub.ErrBadCode.Error())
	case errors.Is(err, hub.ErrForbidden), errors.Is(err, hub.ErrUnknownBackend):
		s.fail(w, err)
	default:
		s.log.Warn("Claude request failed", "error", err.Error())
		errorJSON(w, http.StatusBadGateway, "Could not reach the Claude accounts plugin on Hermes")
	}
}

func (s *Server) handleClaudeAccounts(w http.ResponseWriter, r *http.Request) {
	if s.claude == nil {
		errorJSON(w, http.StatusNotFound, hub.ErrNoClaude.Error())
		return
	}
	out, err := s.claude.ClaudeAccounts(r.Context(), userFrom(r), r.PathValue("backend"), r.URL.Query().Get("usage") == "1")
	if err != nil {
		s.claudeFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleClaudeUsage(w http.ResponseWriter, r *http.Request) {
	if s.claude == nil {
		errorJSON(w, http.StatusNotFound, hub.ErrNoClaude.Error())
		return
	}
	out, err := s.claude.ClaudeUsage(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("account"),
		r.URL.Query().Get("fresh") == "1")
	if err != nil {
		s.claudeFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleClaudeSignIn starts (POST), reads (GET) or cancels (DELETE) an
// account's sign-in. Its answer, with the sign-in URL, goes only to the
// admin who asked.
func (s *Server) handleClaudeSignIn(w http.ResponseWriter, r *http.Request) {
	if s.claude == nil {
		errorJSON(w, http.StatusNotFound, hub.ErrNoClaude.Error())
		return
	}
	user, backend, account := userFrom(r), r.PathValue("backend"), r.PathValue("account")
	var out hub.ClaudeSignIn
	var err error
	switch r.Method {
	case http.MethodPost:
		out, err = s.claude.StartSignIn(r.Context(), user, backend, account)
	case http.MethodDelete:
		out, err = s.claude.CancelSignIn(r.Context(), user, backend, account)
	default:
		out, err = s.claude.SignInState(r.Context(), user, backend, account)
	}
	if err != nil {
		s.claudeFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleClaudeCode passes the code from the Claude sign-in page straight
// to the plugin. It never logs, keeps or echoes the code.
func (s *Server) handleClaudeCode(w http.ResponseWriter, r *http.Request) {
	if s.claude == nil {
		errorJSON(w, http.StatusNotFound, hub.ErrNoClaude.Error())
		return
	}
	var body struct {
		SignInID string `json:"signin_id"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	out, err := s.claude.SendSignInCode(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("account"), body.SignInID, body.Code)
	if err != nil {
		s.claudeFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleClaudePool says which account the Claude pool runs a chat's next
// turn on: ?session=<chat>&model=<model>.
func (s *Server) handleClaudePool(w http.ResponseWriter, r *http.Request) {
	if s.claude == nil {
		errorJSON(w, http.StatusNotFound, hub.ErrNoClaude.Error())
		return
	}
	q := r.URL.Query()
	out, err := s.claude.ClaudePool(r.Context(), userFrom(r), r.PathValue("backend"), q.Get("session"), q.Get("model"))
	if err != nil {
		s.claudeFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
