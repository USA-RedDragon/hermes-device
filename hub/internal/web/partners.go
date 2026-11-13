package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/USA-RedDragon/talaria/hub/internal/agents"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// partnerRoutes adds the API of the chat pairs that work together.
func (s *Server) partnerRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/backends/{backend}/partners", s.api(s.handlePartners))
	mux.Handle("POST /api/backends/{backend}/partners", s.api(s.handleAddPartner))
	mux.Handle("DELETE /api/backends/{backend}/partners/{a}/{b}", s.api(s.handleDeletePartner))
	mux.Handle("GET /api/backends/{backend}/agent-messages", s.api(s.handleAgentMessages))
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
	if err != nil && hub.Unreachable(err) == nil {
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

// maxAgentMessageIDs is the most message ids one call looks up.
const maxAgentMessageIDs = 50

// agentMessageInfo is what the page shows on the card of an agent
// message: its place in the chain and whether sender and receiver work
// together.
type agentMessageInfo struct {
	FromSession string `json:"from_session"`
	Hop         int    `json:"hop"`
	MaxHop      int    `json:"max_hop"`
	Partner     bool   `json:"partner"`
}

// handleAgentMessages answers, for the ids in ?ids= (comma separated)
// that this backend knows, the hop and sender of each message, and with
// ?session= (the chat that got them) whether that chat and the sender
// work together. Only the owner of a single-user backend may ask.
func (s *Server) handleAgentMessages(w http.ResponseWriter, r *http.Request) {
	ps, backend, ok := s.partnerOwner(w, r)
	if !ok {
		return
	}
	if s.agentMessages == nil {
		errorJSON(w, http.StatusNotFound, "This hub keeps no agent messages")
		return
	}
	var ids []string
	for _, id := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) > maxAgentMessageIDs {
		errorJSON(w, http.StatusBadRequest, "Ask for fewer messages at a time")
		return
	}
	var pairs []hub.AgentPartner
	session := r.URL.Query().Get("session")
	if session != "" {
		var err error
		if pairs, err = ps.AgentPartners(r.Context(), userFrom(r), backend); err != nil {
			s.partnerFail(w, err)
			return
		}
	}
	out := map[string]agentMessageInfo{}
	for _, id := range ids {
		m, found, err := s.agentMessages.AgentMessage(r.Context(), backend, id)
		if err != nil {
			s.partnerFail(w, err)
			return
		}
		if !found {
			continue
		}
		info := agentMessageInfo{FromSession: m.FromSession, Hop: m.Hop, MaxHop: agents.MaxHop}
		for _, p := range pairs {
			if m.FromRoot != session && p.Has(m.FromRoot) && p.Has(session) {
				info.Partner, info.MaxHop = true, agents.MaxPartnerHop
			}
		}
		out[id] = info
	}
	writeJSON(w, http.StatusOK, out)
}
