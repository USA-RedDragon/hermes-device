package web

import (
	"errors"
	"net/http"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// claudeCodeRoutes adds the Usage page's Claude Code panel API.
func (s *Server) claudeCodeRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/claude-code/usage", s.api(s.handleClaudeCodeUsage))
	mux.Handle("PUT /api/claude-code/accounts/{account}/name", s.api(s.handleClaudeAccountName))
}

func (s *Server) handleClaudeCodeUsage(w http.ResponseWriter, r *http.Request) {
	if s.claudeCode == nil {
		errorJSON(w, http.StatusNotFound, "Claude Code usage is not set up on this hub")
		return
	}
	q := r.URL.Query()
	out, err := s.claudeCode.ClaudeCodeUsage(r.Context(), userFrom(r), q.Get("range"), q.Get("group"))
	if errors.Is(err, hub.ErrBadRange) {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.log.Error("Claude Code usage", "error", err.Error())
		errorJSON(w, http.StatusInternalServerError, "Could not read Claude Code usage")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleClaudeAccountName(w http.ResponseWriter, r *http.Request) {
	if s.claudeCode == nil {
		errorJSON(w, http.StatusNotFound, "Claude Code usage is not set up on this hub")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &body) {
		return
	}
	err := s.claudeCode.NameClaudeAccount(r.Context(), userFrom(r), r.PathValue("account"), body.Name)
	if errors.Is(err, hub.ErrBadName) {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.log.Error("name Claude account", "error", err.Error())
		errorJSON(w, http.StatusInternalServerError, "Could not save the name")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
