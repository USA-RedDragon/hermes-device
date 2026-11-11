package web

import (
	"bytes"
	"errors"
	"net/http"
	"strconv"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const (
	verbApprove = "approve"
	verbReject  = "reject"
	keyError    = "error"
)

func (s *Server) memoryRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/memory", s.api(s.handleMemoryBackends))
	mux.Handle("GET /api/memory/{backend}/notes", s.api(s.handleNotes))
	mux.Handle("POST /api/memory/{backend}/notes/{target}/{op}", s.api(s.handleChangeNote))
	mux.Handle("POST /api/memory/{backend}/pending/{pending}/{verb}", s.api(s.handlePendingNote))
	mux.Handle("POST /api/memory/{backend}/approval", s.api(s.handleNoteApproval))
	mux.Handle("GET /api/memory/{backend}/learned", s.api(s.handleLearned))
	mux.Handle("POST /api/memory/{backend}/learned", s.api(s.handleAddLearned))
	mux.Handle("GET /api/memory/{backend}/learned/info", s.api(s.handleLearnedInfo))
	mux.Handle("POST /api/memory/{backend}/learned/delete", s.api(s.handleDeleteLearned))
	mux.Handle("GET /api/memory/{backend}/learned/export", s.api(s.handleExportLearned))
}

func (s *Server) memoryFail(w http.ResponseWriter, err error) {
	var me *hub.MemoryError
	switch {
	case s.memory == nil || errors.Is(err, hub.ErrNoMemory):
		errorJSON(w, http.StatusNotFound, "This backend has no such memory")
	case errors.As(err, &me):
		errorJSON(w, me.Status, me.Message)
	default:
		s.fail(w, err)
	}
}

func (s *Server) handleMemoryBackends(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		writeJSON(w, http.StatusOK, []hub.MemoryBackend{})
		return
	}
	bs, err := s.memory.MemoryBackends(r.Context(), userFrom(r))
	if err != nil {
		s.memoryFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bs)
}

func (s *Server) notesAnswer(w http.ResponseWriter, n hub.Notes, err error) {
	if err != nil {
		s.memoryFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) handleNotes(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		s.memoryFail(w, hub.ErrNoMemory)
		return
	}
	n, err := s.memory.Notes(r.Context(), userFrom(r), r.PathValue("backend"))
	s.notesAnswer(w, n, err)
}

func (s *Server) handleChangeNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Entry   string   `json:"entry"`
		Content string   `json:"content"`
		Base    []string `json:"base"`
	}
	if !decode(w, r, &body) {
		return
	}
	if s.memory == nil {
		s.memoryFail(w, hub.ErrNoMemory)
		return
	}
	n, err := s.memory.ChangeNote(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("target"), r.PathValue("op"), body.Entry, body.Content, body.Base)
	s.notesAnswer(w, n, err)
}

func (s *Server) handlePendingNote(w http.ResponseWriter, r *http.Request) {
	verb := r.PathValue("verb")
	if verb != verbApprove && verb != verbReject {
		errorJSON(w, http.StatusNotFound, "not found")
		return
	}
	if s.memory == nil {
		s.memoryFail(w, hub.ErrNoMemory)
		return
	}
	n, err := s.memory.ResolvePending(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("pending"), verb == verbApprove)
	s.notesAnswer(w, n, err)
}

func (s *Server) handleNoteApproval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Enabled == nil {
		errorJSON(w, http.StatusBadRequest, "enabled is required")
		return
	}
	if s.memory == nil {
		s.memoryFail(w, hub.ErrNoMemory)
		return
	}
	n, err := s.memory.SetApproval(r.Context(), userFrom(r), r.PathValue("backend"), *body.Enabled)
	s.notesAnswer(w, n, err)
}

func (s *Server) handleLearned(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		s.memoryFail(w, hub.ErrNoMemory)
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	p, err := s.memory.Learned(r.Context(), userFrom(r), r.PathValue("backend"),
		hub.LearnedQuery{View: q.Get("view"), Query: q.Get("q"), Page: page, Oldest: q.Get("order") == "oldest", All: q.Get("show") == "all"})
	if err != nil {
		s.memoryFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleLearnedInfo(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		s.memoryFail(w, hub.ErrNoMemory)
		return
	}
	info, err := s.memory.LearnedInfo(r.Context(), userFrom(r), r.PathValue("backend"))
	if err != nil {
		s.memoryFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleAddLearned(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if !decode(w, r, &body) {
		return
	}
	if s.memory == nil {
		s.memoryFail(w, hub.ErrNoMemory)
		return
	}
	c, err := s.memory.AddLearned(r.Context(), userFrom(r), r.PathValue("backend"), body.Content)
	if err != nil {
		s.memoryFail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleDeleteLearned(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &body) {
		return
	}
	if s.memory == nil {
		s.memoryFail(w, hub.ErrNoMemory)
		return
	}
	n, err := s.memory.DeleteLearned(r.Context(), userFrom(r), r.PathValue("backend"), body.IDs)
	if err != nil {
		var me *hub.MemoryError
		if n > 0 && errors.As(err, &me) {
			writeJSON(w, me.Status, map[string]any{keyError: me.Message, "deleted": n})
			return
		}
		s.memoryFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
}

func (s *Server) handleExportLearned(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		s.memoryFail(w, hub.ErrNoMemory)
		return
	}
	backend := r.PathValue("backend")
	var buf bytes.Buffer
	if err := s.memory.ExportLearned(r.Context(), userFrom(r), backend, &buf); err != nil {
		s.memoryFail(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", `attachment; filename="hermes-learned.json"`)
	_, _ = w.Write(buf.Bytes())
}
