package web

import (
	"errors"
	"net/http"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// partnerRoutes adds the API of the chat pairs that work together.
func (s *Server) partnerRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/backends/{backend}/partners", s.api(s.handlePartners))
	mux.Handle("POST /api/backends/{backend}/partners", s.api(s.handleAddPartner))
	mux.Handle("DELETE /api/backends/{backend}/partners/{a}/{b}", s.api(s.handleDeletePartner))
}

// partnerOwner answers 404 when the hub keeps no pairs or the backend has
// no owner, and 403 unless the signed-in user owns the backend.
func (s *Server) partnerOwner(w http.ResponseWriter, r *http.Request) (hub.AgentPartners, string, bool) {
	backend := r.PathValue("backend")
	owner, known := s.partnerOwners[backend]
	if s.partners == nil || !known {
		errorJSON(w, http.StatusNotFound, "This backend keeps no chat pairs that work together")
		return nil, "", false
	}
	if owner != userFrom(r) {
		errorJSON(w, http.StatusForbidden, "Only the owner of this backend can choose which chats work together")
		return nil, "", false
	}
	return s.partners, backend, true
}

// handlePartners lists the pairs of a backend, or with ?session= the
// chats that work with that one.
func (s *Server) handlePartners(w http.ResponseWriter, r *http.Request) {
	ps, backend, ok := s.partnerOwner(w, r)
	if !ok {
		return
	}
	pairs, err := ps.AgentPartners(r.Context(), userFrom(r), backend)
	if err != nil {
		s.partnerFail(w, err)
		return
	}
	if id := r.URL.Query().Get("session"); id != "" {
		kept := pairs[:0]
		for _, p := range pairs {
			if p.Has(id) {
				kept = append(kept, p)
			}
		}
		pairs = kept
	}
	writeJSON(w, http.StatusOK, pairs)
}

func (s *Server) handleAddPartner(w http.ResponseWriter, r *http.Request) {
	ps, backend, ok := s.partnerOwner(w, r)
	if !ok {
		return
	}
	var body hub.AgentPartner
	if !decode(w, r, &body) {
		return
	}
	if body.A == "" || body.B == "" || body.A == body.B {
		errorJSON(w, http.StatusBadRequest, "Name two different chats")
		return
	}
	sessions, err := s.svc.Sessions(r.Context(), userFrom(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	found := 0
	for _, x := range sessions {
		if x.BackendID == backend && (x.ID == body.A || x.ID == body.B) {
			found++
		}
	}
	if found != 2 {
		errorJSON(w, http.StatusNotFound, "One of those chats no longer exists on this backend")
		return
	}
	p := hub.NewAgentPartner(body.A, body.B)
	if err := ps.AddAgentPartner(r.Context(), userFrom(r), backend, p); err != nil {
		s.partnerFail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleDeletePartner(w http.ResponseWriter, r *http.Request) {
	ps, backend, ok := s.partnerOwner(w, r)
	if !ok {
		return
	}
	found, err := ps.RemoveAgentPartner(r.Context(), userFrom(r), backend, hub.NewAgentPartner(r.PathValue("a"), r.PathValue("b")))
	switch {
	case err != nil:
		s.partnerFail(w, err)
	case !found:
		errorJSON(w, http.StatusNotFound, "Those chats were not marked as working together")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) partnerFail(w http.ResponseWriter, err error) {
	if errors.Is(err, hub.ErrTooManyAgentPartners) {
		errorJSON(w, http.StatusConflict, "You have the most pairs the hub keeps. Remove one first")
		return
	}
	s.log.Error("agent partners", "error", err.Error())
	errorJSON(w, http.StatusInternalServerError, "Could not read or save the chats that work together. Try again")
}
