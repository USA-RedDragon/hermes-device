package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

type backendJSON struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

type sessionJSON struct {
	ID        string    `json:"id"`
	BackendID string    `json:"backend_id"`
	Title     string    `json:"title"`
	Live      bool      `json:"live"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toJSON(s hub.Session) sessionJSON {
	return sessionJSON{ID: s.ID, BackendID: s.BackendID, Title: s.Title, Live: s.Live, UpdatedAt: s.UpdatedAt}
}

func (s *Server) handleBackends(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r)
	bs, err := s.svc.Backends(r.Context(), user)
	if err != nil {
		s.fail(w, err)
		return
	}
	status := map[string]string{}
	if sr, ok := s.svc.(StatusReporter); ok {
		status, _ = sr.BackendStatus(r.Context(), user)
	}
	out := make([]backendJSON, 0, len(bs))
	for _, b := range bs {
		out = append(out, backendJSON{ID: b.ID, Name: b.Name, Error: status[b.ID]})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	ss, err := s.svc.Sessions(r.Context(), userFrom(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]sessionJSON, 0, len(ss))
	for _, x := range ss {
		out = append(out, toJSON(x))
	}
	writeJSON(w, http.StatusOK, out)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Header.Get("Content-Type") != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "JSON body required"})
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad JSON body"})
		return false
	}
	return true
}

func (s *Server) handleNewSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BackendID string `json:"backend_id"`
	}
	if !decode(w, r, &body) {
		return
	}
	sess, err := s.svc.NewSession(r.Context(), userFrom(r), body.BackendID)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toJSON(sess))
}

func (s *Server) handlePrompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty prompt"})
		return
	}
	if err := s.svc.Prompt(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), body.Text); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Interrupt(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allow bool `json:"allow"`
	}
	if !decode(w, r, &body) {
		return
	}
	err := s.svc.RespondApproval(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"),
		r.PathValue("approval"), body.Allow)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleEvents streams a session as server-sent events. Each event's id is
// the hub sequence, so a browser that reconnects sends it back as
// Last-Event-ID and gets only what it missed.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	since := r.Header.Get("Last-Event-ID")
	if since == "" {
		since = r.URL.Query().Get("since")
	}
	sinceSeq, _ := strconv.ParseInt(since, 10, 64)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, stop, err := s.svc.Attach(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), sinceSeq)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer stop()
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()
	tick := time.NewTicker(s.keepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(map[string]any{"kind": e.Kind, "data": e.Data})
			if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
