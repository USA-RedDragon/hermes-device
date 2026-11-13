package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
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
	Activity     int       `json:"activity,omitempty"`
	Pinned       bool      `json:"pinned"`
	PinPos       int64     `json:"pin_pos"`
	Unread       bool      `json:"unread"`
	Needs        string    `json:"needs"`
	Page         bool      `json:"page"`
}

func toJSON(s hub.Session) sessionJSON {
	return sessionJSON{ID: s.ID, BackendID: s.BackendID, Title: s.Title, Live: s.Live,
		Running: s.Running, Archived: s.Archived, MessageCount: s.MessageCount, UpdatedAt: s.UpdatedAt, Source: s.Source,
		Activity: s.Activity, Needs: s.Needs}
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
	user := userFrom(r)
	ss, err := s.svc.Sessions(hub.WithCached(r.Context()), user)
	ue := hub.Unreachable(err)
	if err != nil && ue == nil {
		s.fail(w, err)
		return
	}
	if ue != nil && len(ue.Pending) > 0 {
		// The hub's copy of these backends' chats, while their fresh lists
		// are on the way: the page shows it and asks again soon.
		w.Header().Set(RefreshingHeader, idList(ue.Pending))
	}
	if ue != nil && len(ue.Backends) > 0 {
		// The page must never take a failed list for an empty one: it
		// keeps the chats it has for these backends, says so and retries.
		w.Header().Set(UnreachableHeader, idList(ue.Backends))
		if len(ss) == 0 {
			errorJSON(w, http.StatusServiceUnavailable, "Cannot reach the agent ("+unreachableWords(ue)+"). Retrying")
			return
		}
	}
	writeJSON(w, http.StatusOK, s.marked(r.Context(), user, ss))
}

// UnreachableHeader names, comma separated and query-escaped, the backends
// a session list could not reach. Their rows in the answer, if any, are
// the hub's last list of them.
const UnreachableHeader = "X-Talaria-Unreachable"

// RefreshingHeader names, as UnreachableHeader does, the backends whose
// rows are the hub's copy because their fresh list is still on its way.
const RefreshingHeader = "X-Talaria-Refreshing"

// idList is the keys of m, query-escaped, sorted and comma separated.
func idList[V any](m map[string]V) string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, url.QueryEscape(id))
	}
	slices.Sort(ids)
	return strings.Join(ids, ",")
}

func unreachableWords(ue *hub.UnreachableError) string {
	seen := map[string]bool{}
	var out []string
	for _, why := range ue.Backends {
		if !seen[why] {
			seen[why] = true
			out = append(out, why)
		}
	}
	slices.Sort(out)
	return strings.Join(out, "; ")
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
	text, files, steer, page, id, ok := promptBody(w, r)
	if !ok {
		return
	}
	if id != "" && !hub.ValidClientID(id) {
		errorJSON(w, http.StatusBadRequest, "bad message id")
		return
	}
	if page != nil {
		var err error
		if text, err = hub.WithPageContext(text, *page); err != nil {
			errorJSON(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if text == "" && len(files) == 0 {
		errorJSON(w, http.StatusBadRequest, "empty prompt")
		return
	}
	ctx := r.Context()
	if id != "" {
		ctx = hub.WithSubmissionID(ctx, id)
	}
	if steer {
		ctx = hub.WithSteer(ctx)
	}
	res, err := s.svc.Prompt(ctx, userFrom(r), r.PathValue("backend"), r.PathValue("session"), text, files...)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toPromptJSON(res))
}

// maxPromptText bounds a prompt's text, maxFileName a file name.
const (
	maxPromptText = 1 << 20
	maxFileName   = 4096
)

// promptBody reads a prompt: JSON {"text", "steer", "page", "id"}, or
// multipart/form-data with an "id" and a "text" field and one "file" part per
// attachment. A "pages" field after a file part picks the pages of that PDF
// to show the agent: "N", "N-M" or "N-". A "steer" field of "true" steers
// like the JSON flag; its files then go to the agent as saved files, since
// Hermes steers text only. Only a JSON prompt carries the page chat's
// context. The id is the page's key for the message:
// the same id sent again never runs the prompt twice.
func promptBody(w http.ResponseWriter, r *http.Request) (string, []hub.Attachment, bool, *hub.PageContext, string, bool) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		var body struct {
			Text  string           `json:"text"`
			Steer bool             `json:"steer"`
			Page  *hub.PageContext `json:"page"`
			ID    string           `json:"id"`
		}
		ok := decode(w, r, &body)
		return body.Text, nil, body.Steer, body.Page, body.ID, ok
	}
	text, files, id, steer, ok := formPrompt(w, r)
	return text, files, steer, nil, id, ok
}

// formPrompt reads a multipart/form-data prompt.
func formPrompt(w http.ResponseWriter, r *http.Request) (string, []hub.Attachment, string, bool, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, hub.MaxAttachmentBytes+maxPromptText+1<<20)
	mr, err := r.MultipartReader()
	if err != nil {
		errorJSON(w, http.StatusBadRequest, "bad form body")
		return "", nil, "", false, false
	}
	var text, id string
	var steer bool
	var files []hub.Attachment
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return text, files, id, steer, true
		}
		if err != nil {
			bodyError(w, err)
			return "", nil, "", false, false
		}
		switch part.FormName() {
		case "id":
			b, _ := io.ReadAll(io.LimitReader(part, hub.MaxClientID+1))
			id = string(b)
		case "steer":
			b, _ := io.ReadAll(io.LimitReader(part, 6))
			steer = string(b) == "true"
		case "text":
			b, err := io.ReadAll(io.LimitReader(part, maxPromptText+1))
			if err != nil || len(b) > maxPromptText {
				bodyError(w, err)
				return "", nil, "", false, false
			}
			text = string(b)
		case "file":
			if len(files) >= hub.MaxAttachments {
				errorJSON(w, http.StatusBadRequest, fmt.Sprintf("Attach at most %d files to one message", hub.MaxAttachments))
				return "", nil, "", false, false
			}
			raw := part.FileName()
			if len(raw) > maxFileName {
				errorJSON(w, http.StatusBadRequest, "A file name is too long")
				return "", nil, "", false, false
			}
			name := hub.CleanName(raw)
			b, err := io.ReadAll(io.LimitReader(part, hub.MaxFileBytes+1))
			if err != nil {
				bodyError(w, err)
				return "", nil, "", false, false
			}
			if len(b) > hub.MaxFileBytes {
				errorJSON(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("%s is larger than %d MB, the most the hub takes for a file", name, hub.MaxFileBytes>>20))
				return "", nil, "", false, false
			}
			files = append(files, hub.Attachment{Name: name, Data: b})
		case "pages":
			b, err := io.ReadAll(io.LimitReader(part, 33))
			from, to, ok := pageRange(string(b))
			if err != nil || !ok || len(b) > 32 || len(files) == 0 {
				errorJSON(w, http.StatusBadRequest, "Pick pages as a number or a range such as 3-10")
				return "", nil, "", false, false
			}
			files[len(files)-1].From, files[len(files)-1].To = from, to
		default:
			errorJSON(w, http.StatusBadRequest, "unknown form field")
			return "", nil, "", false, false
		}
		_ = part.Close()
	}
}

// pageRange reads "N", "N-M" or "N-" with 1 <= N <= M. An empty string
// is every page.
func pageRange(s string) (int, int, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	if s == "" {
		return 0, 0, true
	}
	a, b, isRange := strings.Cut(s, "-")
	from, err := strconv.Atoi(a)
	if err != nil || from < 1 || from > maxPage {
		return 0, 0, false
	}
	if !isRange {
		return from, from, true
	}
	if b == "" {
		return from, 0, true
	}
	to, err := strconv.Atoi(b)
	if err != nil || to < from || to > maxPage {
		return 0, 0, false
	}
	return from, to, true
}

// maxPage bounds a page number in a range.
const maxPage = 1 << 20

func bodyError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		errorJSON(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("The attachments add up to more than %d MB", hub.MaxAttachmentBytes>>20))
		return
	}
	errorJSON(w, http.StatusBadRequest, "bad form body")
}

// Prompt statuses: sent to the backend, held until it is back, or a slash
// command that Hermes ran at once.
const (
	promptSent    = "sent"
	promptHeld    = "held"
	promptCommand = "command"
)

type promptJSON struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	HeldUntil time.Time `json:"held_until,omitzero"`
	Notice    string    `json:"notice,omitempty"`
	Steered   bool      `json:"steered"`
	Output    string    `json:"output,omitempty"`
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

// handleStopAndSend stops the running turn and runs a waiting input as the
// next turn (see hub.Redirector).
func (s *Server) handleStopAndSend(w http.ResponseWriter, r *http.Request) {
	rd, ok := s.svc.(hub.Redirector)
	if !ok {
		errorJSON(w, http.StatusNotFound, "not available")
		return
	}
	res, err := rd.StopAndSend(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), r.PathValue("input"))
	if errors.Is(err, hub.ErrBusy) {
		errorJSON(w, http.StatusConflict, "That message is the turn that runs now. Use Stop instead")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toPromptJSON(res))
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

// freshModels is the optional part of a hub.Service that lists a chat's
// models without the backend's cache.
type freshModels interface {
	ModelsRefresh(ctx context.Context, user, backendID, sessionID string) (hub.Models, error)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	list := s.svc.Models
	if f, ok := s.svc.(freshModels); ok && r.URL.Query().Get("refresh") == "1" {
		list = f.ModelsRefresh
	}
	m, err := list(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"))
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
	At   int64           `json:"at,omitempty"`
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
	ew := &eventWriter{s: s, split: since == ""}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// While the backend is slow to answer, a page that opens the chat gets
	// the hub's copy of it first.
	ch, stop, err := s.svc.Attach(hub.WithCached(r.Context()), userFrom(r), r.PathValue("backend"), r.PathValue("session"), sinceSeq)
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
	h.Add("Vary", "Accept-Encoding")
	out := newStreamWriter(w, flusher, r.Header.Get("Accept-Encoding"))
	defer out.Close()
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(out, "retry: 2000\n\n")
	out.Flush()
	tick := time.NewTicker(s.keepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.draining:
			s.flushEvents(out, ch)
			_, _ = fmt.Fprintf(out, "retry: %d\ndata: {\"kind\":%q}\n\n", reconnectRetry.Milliseconds(), kindReconnect)
			out.Flush()
			return
		case <-tick.C:
			if _, err := fmt.Fprint(out, ": keepalive\n\n"); err != nil {
				return
			}
			out.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			if !ew.write(out, e) {
				return
			}
			out.Flush()
		}
	}
}

// flushEvents writes the events already waiting on ch.
func (s *Server) flushEvents(w io.Writer, ch <-chan hub.Event) {
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

func (s *Server) writeEvent(w io.Writer, e hub.Event) bool {
	data, err := json.Marshal(sseEvent{Kind: e.Kind, Data: e.Data, At: e.At})
	if err != nil {
		return true
	}
	_, err = fmt.Fprintf(w, "id: %s.%d\ndata: %s\n\n", s.instance, e.Seq, data)
	return err == nil
}
