package web

import (
	"errors"
	"net/http"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// handleSubagents reads (GET) or sets (PUT {"choice"}) a chat's
// Subagents setting. A backend without the subagent-routes plugin answers
// 404, and the page then shows no Subagents picker.
func (s *Server) handleSubagents(w http.ResponseWriter, r *http.Request) {
	if s.subagents == nil {
		errorJSON(w, http.StatusNotFound, hub.ErrNoSubagentRoutes.Error())
		return
	}
	backend, session := r.PathValue("backend"), r.PathValue("session")
	var out hub.Subagents
	var err error
	if r.Method == http.MethodPut {
		var body struct {
			Choice string `json:"choice"`
		}
		if !decode(w, r, &body) {
			return
		}
		out, err = s.subagents.SetChatSubagents(r.Context(), userFrom(r), backend, session, body.Choice)
	} else {
		out, err = s.subagents.ChatSubagents(r.Context(), userFrom(r), backend, session)
	}
	switch {
	case errors.Is(err, hub.ErrNoSubagentRoutes):
		errorJSON(w, http.StatusNotFound, hub.ErrNoSubagentRoutes.Error())
	case err != nil:
		s.fail(w, err)
	default:
		writeJSON(w, http.StatusOK, out)
	}
}
