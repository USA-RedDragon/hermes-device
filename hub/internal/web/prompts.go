package web

import (
	"errors"
	"net/http"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// promptRoutes adds the API of each user's saved prompts.
func (s *Server) promptRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/prompts", s.api(s.handlePrompts))
	mux.Handle("POST /api/prompts", s.api(s.handleAddPrompt))
	mux.Handle("PUT /api/prompts/{name}", s.api(s.handleUpdatePrompt))
	mux.Handle("DELETE /api/prompts/{name}", s.api(s.handleDeletePrompt))
}

func (s *Server) promptLibrary(w http.ResponseWriter) (hub.PromptLibrary, bool) {
	if s.prompts == nil {
		errorJSON(w, http.StatusNotFound, "This hub keeps no prompts")
		return nil, false
	}
	return s.prompts, true
}

func (s *Server) handlePrompts(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.promptLibrary(w)
	if !ok {
		return
	}
	ps, err := lib.Prompts(r.Context(), userFrom(r))
	if err != nil {
		s.promptFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

// readPrompt decodes and checks a prompt from the request body.
func readPrompt(w http.ResponseWriter, r *http.Request) (hub.SavedPrompt, bool) {
	var body struct {
		Name string `json:"name"`
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return hub.SavedPrompt{}, false
	}
	p := hub.SavedPrompt{Name: body.Name, Text: body.Text}
	if err := hub.ValidatePrompt(p); err != nil {
		errorJSON(w, http.StatusBadRequest, hub.PromptReason(err))
		return hub.SavedPrompt{}, false
	}
	return p, true
}

func (s *Server) handleAddPrompt(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.promptLibrary(w)
	if !ok {
		return
	}
	p, ok := readPrompt(w, r)
	if !ok {
		return
	}
	if err := lib.AddPrompt(r.Context(), userFrom(r), p); err != nil {
		s.promptFail(w, err)
		return
	}
	s.writePrompt(w, r, lib, p.Name, http.StatusCreated)
}

func (s *Server) handleUpdatePrompt(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.promptLibrary(w)
	if !ok {
		return
	}
	p, ok := readPrompt(w, r)
	if !ok {
		return
	}
	if err := lib.UpdatePrompt(r.Context(), userFrom(r), r.PathValue("name"), p); err != nil {
		s.promptFail(w, err)
		return
	}
	s.writePrompt(w, r, lib, p.Name, http.StatusOK)
}

// writePrompt answers with the prompt as the store keeps it.
func (s *Server) writePrompt(w http.ResponseWriter, r *http.Request, lib hub.PromptLibrary, name string, code int) {
	p, ok, err := lib.Prompt(r.Context(), userFrom(r), name)
	if err == nil && !ok {
		err = hub.ErrNoPrompt
	}
	if err != nil {
		s.promptFail(w, err)
		return
	}
	writeJSON(w, code, p)
}

func (s *Server) handleDeletePrompt(w http.ResponseWriter, r *http.Request) {
	lib, ok := s.promptLibrary(w)
	if !ok {
		return
	}
	if err := lib.DeletePrompt(r.Context(), userFrom(r), r.PathValue("name")); err != nil {
		s.promptFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) promptFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hub.ErrPromptExists):
		errorJSON(w, http.StatusConflict, "You already have a prompt with that name. Pick another name")
	case errors.Is(err, hub.ErrNoPrompt):
		errorJSON(w, http.StatusNotFound, "That prompt no longer exists. It may have been deleted on another device")
	case errors.Is(err, hub.ErrTooManyPrompts):
		errorJSON(w, http.StatusConflict, "You have the most prompts the hub keeps. Delete one first")
	default:
		s.log.Error("saved prompts", "error", err.Error())
		errorJSON(w, http.StatusInternalServerError, "Could not read or save your prompts. Try again")
	}
}
