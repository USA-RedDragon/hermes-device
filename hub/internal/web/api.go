package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

type backendJSON struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Error string `json:"error,omitempty"`
}

type sessionJSON struct {
	ID           string    `json:"id"`
	BackendID    string    `json:"backend_id"`
	Title        string    `json:"title"`
	Live         bool      `json:"live"`
	Running      bool      `json:"running"`
	Archived     bool      `json:"archived"`
	MessageCount int       `json:"message_count"`
	UpdatedAt    time.Time `json:"updated_at"`
	Source       string    `json:"source,omitempty"`
}

func toJSON(s hub.Session) sessionJSON {
	return sessionJSON{ID: s.ID, BackendID: s.BackendID, Title: s.Title, Live: s.Live,
		Running: s.Running, Archived: s.Archived, MessageCount: s.MessageCount, UpdatedAt: s.UpdatedAt, Source: s.Source}
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
		errorJSON(w, http.StatusUnsupportedMediaType, "JSON body required")
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		errorJSON(w, http.StatusBadRequest, "bad JSON body")
		return false
	}
	return true
}

func (s *Server) handleNewSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BackendID string `json:"backend_id"`
		DeviceID  string `json:"device_id"`
		Dir       string `json:"dir"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.DeviceID != "" {
		s.startOn(w, r, body.DeviceID, body.Dir)
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
	text, files, ok := promptBody(w, r)
	if !ok {
		return
	}
	if text == "" && len(files) == 0 {
		errorJSON(w, http.StatusBadRequest, "empty prompt")
		return
	}
	res, err := s.svc.Prompt(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), text, files...)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := promptJSON{ID: res.ID, Status: promptSent, Notice: res.Notice}
	if res.Held {
		out.Status, out.HeldUntil = promptHeld, res.HeldUntil
	}
	writeJSON(w, http.StatusAccepted, out)
}

// maxPromptText bounds a prompt's text.
const maxPromptText = 1 << 20

// promptBody reads a prompt: JSON {"text"}, or multipart/form-data with a
// "text" field and one "file" part per attachment.
func promptBody(w http.ResponseWriter, r *http.Request) (string, []hub.Attachment, bool) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		var body struct {
			Text string `json:"text"`
		}
		return body.Text, nil, decode(w, r, &body)
	}
	r.Body = http.MaxBytesReader(w, r.Body, hub.MaxAttachmentBytes+maxPromptText+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		errorJSON(w, http.StatusBadRequest, "bad form body")
		return "", nil, false
	}
	var text string
	var files []hub.Attachment
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return text, files, true
		}
		if err != nil {
			bodyError(w, err)
			return "", nil, false
		}
		switch part.FormName() {
		case "text":
			b, err := io.ReadAll(io.LimitReader(part, maxPromptText+1))
			if err != nil || len(b) > maxPromptText {
				bodyError(w, err)
				return "", nil, false
			}
			text = string(b)
		case "file":
			if len(files) >= hub.MaxAttachments {
				errorJSON(w, http.StatusBadRequest, fmt.Sprintf("Attach at most %d files to one message", hub.MaxAttachments))
				return "", nil, false
			}
			name := hub.CleanName(part.FileName())
			b, err := io.ReadAll(io.LimitReader(part, hub.MaxFileBytes+1))
			if err != nil {
				bodyError(w, err)
				return "", nil, false
			}
			if len(b) > hub.MaxFileBytes {
				errorJSON(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("%s is larger than %d MB, the most the hub takes for a file", name, hub.MaxFileBytes>>20))
				return "", nil, false
			}
			files = append(files, hub.Attachment{Name: name, Data: b})
		default:
			errorJSON(w, http.StatusBadRequest, "unknown form field")
			return "", nil, false
		}
		_ = part.Close()
	}
}

func bodyError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		errorJSON(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("The attachments add up to more than %d MB", hub.MaxAttachmentBytes>>20))
		return
	}
	errorJSON(w, http.StatusBadRequest, "bad form body")
}

// Prompt statuses: sent to the backend, or held until it is back.
const (
	promptSent = "sent"
	promptHeld = "held"
)

type promptJSON struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	HeldUntil time.Time `json:"held_until,omitzero"`
	Notice    string    `json:"notice,omitempty"`
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

func (s *Server) handleCancelInput(w http.ResponseWriter, r *http.Request) {
	s.inputAction(w, r, s.svc.CancelInput)
}

func (s *Server) handleDismissLost(w http.ResponseWriter, r *http.Request) {
	s.inputAction(w, r, s.svc.DismissLost)
}

func (s *Server) inputAction(w http.ResponseWriter, r *http.Request,
	fn func(ctx context.Context, user, backendID, sessionID, inputID string) error) {
	if err := fn(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), r.PathValue("input")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.done(w, s.svc.RenameSession(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), body.Title))
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Archived bool `json:"archived"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.done(w, s.svc.ArchiveSession(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), body.Archived))
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	s.done(w, s.svc.DeleteSession(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session")))
}

type modelsJSON struct {
	hub.Models
	Efforts []string `json:"efforts"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.Models(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, modelsJSON{Models: m, Efforts: hub.ReasoningEfforts()})
}

func (s *Server) handleSetModel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Confirm  string `json:"confirm"`
	}
	if !decode(w, r, &body) {
		return
	}
	res, err := s.svc.SetModel(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"),
		body.Provider, body.Model, body.Confirm)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSetReasoning(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Effort string `json:"effort"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.done(w, s.svc.SetReasoning(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), body.Effort))
}

// done answers 202 for a change the service made, or the error.
func (s *Server) done(w http.ResponseWriter, err error) {
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// kindReconnect tells the page that the hub is shutting down and its
// stream should reconnect, to another replica. It carries no event id, so
// the page sends the last id it got, which another replica reads as 0.
const kindReconnect = "hub.reconnect"

// reconnectRetry is how soon the page reconnects after kindReconnect.
const reconnectRetry = 250 * time.Millisecond

type sseEvent struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// handleEvents streams a session as server-sent events. Each event's id is
// the hub sequence, so a browser that reconnects sends it back as
// Last-Event-ID and gets only what it missed.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	since := r.Header.Get("Last-Event-ID")
	if since == "" {
		since = r.URL.Query().Get("since")
	}
	sinceSeq := s.sinceSeq(since)
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
		case <-s.draining:
			s.flushEvents(w, ch)
			_, _ = fmt.Fprintf(w, "retry: %d\ndata: {\"kind\":%q}\n\n", reconnectRetry.Milliseconds(), kindReconnect)
			flusher.Flush()
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
			if !s.writeEvent(w, e) {
				return
			}
			flusher.Flush()
		}
	}
}

// flushEvents writes the events already waiting on ch.
func (s *Server) flushEvents(w http.ResponseWriter, ch <-chan hub.Event) {
	for {
		select {
		case e, ok := <-ch:
			if !ok || !s.writeEvent(w, e) {
				return
			}
		default:
			return
		}
	}
}

// sinceSeq reads a Last-Event-ID. An id is "<instance>.<seq>": sequences
// count per hub process, so an id from another replica, or from before a
// restart, reads as 0 and the stream starts with a fresh snapshot.
func (s *Server) sinceSeq(id string) int64 {
	inst, seq, ok := strings.Cut(id, ".")
	if !ok || inst != s.instance {
		return 0
	}
	n, _ := strconv.ParseInt(seq, 10, 64)
	return n
}

func (s *Server) writeEvent(w http.ResponseWriter, e hub.Event) bool {
	data, err := json.Marshal(sseEvent{Kind: e.Kind, Data: e.Data})
	if err != nil {
		return true
	}
	_, err = fmt.Fprintf(w, "id: %s.%d\ndata: %s\n\n", s.instance, e.Seq, data)
	return err == nil
}
