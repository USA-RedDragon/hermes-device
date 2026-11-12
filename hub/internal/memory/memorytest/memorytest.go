// Package memorytest has fakes of the hermes-device plugin's memory routes
// and of the Honcho API, for tests.
package memorytest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// Request is a request a fake got.
type Request struct {
	Method, Path, Query, Auth string
	Body                      map[string]any
}

type recorder struct {
	mu   sync.Mutex
	reqs []Request
}

func (r *recorder) record(req *http.Request) map[string]any {
	var body map[string]any
	b, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(b, &body)
	r.mu.Lock()
	r.reqs = append(r.reqs, Request{Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery, Auth: req.Header.Get("Authorization"), Body: body})
	r.mu.Unlock()
	return body
}

// Requests returns the requests so far.
func (r *recorder) Requests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reqs)
}

const (
	keyDetail = "detail"
	keyError  = "error"
	keyMsg    = "message"
	notFound  = "Not Found"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

// Plugin fakes a backend's dashboard: the device plugin's memory routes and
// PUT /api/config. It keeps notes in memory and applies changes simply.
type Plugin struct {
	recorder
	srv *httptest.Server
	// Missing makes every memory route answer 404 as a dashboard without
	// them does.
	Missing bool
	// NoProjects makes every project-memory route answer 404 as a
	// dashboard without that plugin does.
	NoProjects bool
	// ProjectPending maps the id of a pending write to the identity of the
	// project it is for, as the project-memory plugin's /pending lists it.
	ProjectPending map[string]string
	// Refuse makes changes answer 409 with this message.
	Refuse   string
	mu       sync.Mutex
	notes    hub.Notes
	projects []hub.ProjectNotes
}

// AddProject adds a project whose MEMORY.md holds entries.
func (p *Plugin) AddProject(pr hub.Project, entries ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entries == nil {
		entries = []string{}
	}
	p.projects = append(p.projects, hub.ProjectNotes{Project: pr, Notes: hub.Notes{Targets: map[string]hub.NoteStore{
		hub.TargetMemory: {File: "/data/projects/" + pr.ID + "/MEMORY.md", Limit: 3500, Enabled: true, Entries: entries},
		hub.TargetUser:   {Entries: []string{}},
	}, Pending: []hub.PendingNote{}}})
}

// NewPlugin starts a fake with notes.
func NewPlugin(notes hub.Notes) *Plugin {
	p := &Plugin{notes: notes}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	return p
}

// URL is the fake's base URL.
func (p *Plugin) URL() string { return p.srv.URL }

// Close stops the fake.
func (p *Plugin) Close() { p.srv.Close() }

// Request sends one request to the fake, like *gateway.Dialer.
func (p *Plugin) Request(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, p.srv.URL+path, r)
	if err != nil {
		return nil, err
	}
	return p.srv.Client().Do(req)
}

func (p *Plugin) serve(w http.ResponseWriter, r *http.Request) {
	body := p.record(r)
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Method == http.MethodPut && r.URL.Path == "/api/config" {
		cfg, _ := body["config"].(map[string]any)
		mem, _ := cfg["memory"].(map[string]any)
		on, ok := mem["write_approval"].(bool)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{keyDetail: "bad"})
			return
		}
		p.notes.Approval = on
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	if r.URL.Path == "/api/plugins/project-memory/pending" {
		if p.NoProjects {
			writeJSON(w, http.StatusNotFound, map[string]string{keyDetail: notFound})
			return
		}
		list := []map[string]string{}
		for id, identity := range p.ProjectPending {
			list = append(list, map[string]string{"id": id, "project": "slug", "identity": identity})
		}
		writeJSON(w, http.StatusOK, map[string]any{"pending": list})
		return
	}
	if rest, ok := strings.CutPrefix(r.URL.Path, "/api/plugins/project-memory/projects"); ok {
		p.serveProject(w, r.Method, rest, body)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/plugins/device/memory")
	if !ok || p.Missing {
		writeJSON(w, http.StatusNotFound, map[string]string{keyDetail: notFound})
		return
	}
	if r.Method == http.MethodGet && rest == "" {
		writeJSON(w, http.StatusOK, p.notes)
		return
	}
	if p.Refuse != "" {
		writeJSON(w, http.StatusConflict, map[string]string{keyError: "over_limit", keyMsg: p.Refuse})
		return
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	if len(parts) == 3 && parts[0] == "pending" {
		i := slices.IndexFunc(p.notes.Pending, func(x hub.PendingNote) bool { return x.ID == parts[1] })
		if i < 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{keyError: "unknown_pending", keyMsg: "no such pending memory write"})
			return
		}
		rec := p.notes.Pending[i]
		p.notes.Pending = slices.Delete(p.notes.Pending, i, i+1)
		if parts[2] == "approve" {
			st := p.notes.Targets[rec.Target]
			for _, c := range rec.Changes {
				st.Entries = append(st.Entries, c.Content)
			}
			p.notes.Targets[rec.Target] = st
		}
		writeJSON(w, http.StatusOK, p.notes)
		return
	}
	if len(parts) != 2 {
		writeJSON(w, http.StatusNotFound, map[string]string{keyDetail: notFound})
		return
	}
	st, ok := p.notes.Targets[parts[0]]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{keyError: "unknown_target", keyMsg: "target must be memory or user"})
		return
	}
	if !apply(w, &st, parts[1], body) {
		return
	}
	p.notes.Targets[parts[0]] = st
	writeJSON(w, http.StatusOK, p.notes)
}

// apply makes one change to a store. It answers the refusal itself and
// returns false when it refuses.
func apply(w http.ResponseWriter, st *hub.NoteStore, op string, body map[string]any) bool {
	str := func(k string) string { s, _ := body[k].(string); return s }
	switch op {
	case "add":
		st.Entries = append(st.Entries, str("content"))
	case "edit":
		if i := slices.Index(st.Entries, str("entry")); i >= 0 {
			st.Entries[i] = str("content")
		}
	case "remove":
		st.Entries = slices.DeleteFunc(st.Entries, func(e string) bool { return e == str("entry") })
	case "replace":
		if _, ok := body["base"].([]any); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{keyError: "bad_request", keyMsg: "base is required"})
			return false
		}
		st.Entries = strings.Split(str("text"), "\n§\n")
	}
	return true
}

func (p *Plugin) serveProject(w http.ResponseWriter, method, rest string, body map[string]any) {
	if p.NoProjects {
		writeJSON(w, http.StatusNotFound, map[string]string{keyDetail: notFound})
		return
	}
	if method == http.MethodGet && rest == "" {
		list := make([]hub.Project, 0, len(p.projects))
		for _, pn := range p.projects {
			list = append(list, pn.Project)
		}
		writeJSON(w, http.StatusOK, map[string]any{"projects": list})
		return
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	i := slices.IndexFunc(p.projects, func(x hub.ProjectNotes) bool { return x.Project.ID == parts[0] })
	if i < 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{keyError: "unknown_project", keyMsg: "no such project"})
		return
	}
	pn := &p.projects[i]
	switch {
	case method == http.MethodGet && len(parts) == 1:
	case method == http.MethodPost && len(parts) == 3 && parts[1] == "memory":
		if p.Refuse != "" {
			writeJSON(w, http.StatusConflict, map[string]string{keyError: "over_limit", keyMsg: p.Refuse})
			return
		}
		st := pn.Targets[hub.TargetMemory]
		if !apply(w, &st, parts[2], body) {
			return
		}
		pn.Targets[hub.TargetMemory] = st
		pn.Project.Count = len(st.Entries)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{keyDetail: notFound})
		return
	}
	writeJSON(w, http.StatusOK, pn)
}

// Conclusion is a fake Honcho conclusion.
type Conclusion struct {
	ID         string    `json:"id"`
	Content    string    `json:"content"`
	ObserverID string    `json:"observer_id"`
	ObservedID string    `json:"observed_id"`
	SessionID  *string   `json:"session_id"`
	Level      string    `json:"level"`
	CreatedAt  time.Time `json:"created_at"`
}

// Honcho fakes the Honcho API for one workspace and key.
type Honcho struct {
	recorder
	srv       *httptest.Server
	workspace string
	key       string
	mu        sync.Mutex
	// Items are newest first.
	Items []Conclusion
	Cards map[string][]string
	// MissingPeers makes card reads answer 404, as for an unknown peer.
	MissingPeers bool
	queue        [4]int64
	next         int
	hangUp       bool
}

// NewHoncho starts a fake that accepts key for workspace.
func NewHoncho(workspace, key string) *Honcho {
	h := &Honcho{workspace: workspace, key: key, Cards: map[string][]string{}}
	h.srv = httptest.NewServer(http.HandlerFunc(h.route))
	return h
}

// SetQueue sets the workspace's queue status: total, completed, in
// progress and pending work units.
func (h *Honcho) SetQueue(total, completed, inProgress, pending int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queue = [4]int64{total, completed, inProgress, pending}
}

// URL is the fake's base URL.
func (h *Honcho) URL() string { return h.srv.URL }

// Close stops the fake.
func (h *Honcho) Close() { h.srv.Close() }

// HangUp makes the fake close every connection without an answer, as an
// unreachable Honcho does. Unlike Close, the fake keeps its port, so no
// other server can take the port and answer in its place.
func (h *Honcho) HangUp() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hangUp = true
}

// Add adds an explicit conclusion as the newest.
func (h *Honcho) Add(observer, observed, content, session string) string {
	return h.AddLevel(observer, observed, content, session, "explicit")
}

// AddLevel adds a conclusion of a level as the newest.
func (h *Honcho) AddLevel(observer, observed, content, session, level string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	c := Conclusion{ID: fmt.Sprintf("c%d", h.next), Content: content, ObserverID: observer, ObservedID: observed, Level: level,
		CreatedAt: time.Date(2026, 11, 1, 0, 0, h.next, 0, time.UTC)}
	if session != "" {
		c.SessionID = &session
	}
	h.Items = append([]Conclusion{c}, h.Items...)
	return c.ID
}

// isIn matches a filter value: a string, or {"in": [...]}.
func isIn(v any, have string) bool {
	if m, ok := v.(map[string]any); ok {
		in, _ := m["in"].([]any)
		return slices.Contains(in, any(have))
	}
	want, _ := v.(string)
	return have == want
}

func (h *Honcho) match(c Conclusion, f map[string]any) bool {
	for k, v := range f {
		switch k {
		case "observer_id":
			if !isIn(v, c.ObserverID) {
				return false
			}
		case "observed_id":
			if !isIn(v, c.ObservedID) {
				return false
			}
		case "level":
			if !isIn(v, c.Level) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// route serves the queue status, and every other route with serve.
func (h *Honcho) route(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	hangUp := h.hangUp
	h.mu.Unlock()
	if hangUp {
		panic(http.ErrAbortHandler)
	}
	if r.Method != http.MethodGet || r.URL.Path != "/v3/workspaces/"+h.workspace+"/queue/status" {
		h.serve(w, r)
		return
	}
	h.record(r)
	if r.Header.Get("Authorization") != "Bearer "+h.key {
		writeJSON(w, http.StatusUnauthorized, map[string]string{keyDetail: "bad key"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"total_work_units": h.queue[0], "completed_work_units": h.queue[1],
		"in_progress_work_units": h.queue[2], "pending_work_units": h.queue[3], "sessions": map[string]any{}})
}

func (h *Honcho) serve(w http.ResponseWriter, r *http.Request) {
	body := h.record(r)
	if r.Header.Get("Authorization") != "Bearer "+h.key {
		writeJSON(w, http.StatusUnauthorized, map[string]string{keyDetail: "bad key"})
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v3/workspaces/"+h.workspace)
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{keyDetail: "other workspace"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	filters, _ := body["filters"].(map[string]any)
	switch {
	case r.Method == http.MethodPost && rest == "/conclusions/list":
		var items []Conclusion
		for _, c := range h.Items {
			if h.match(c, filters) {
				items = append(items, c)
			}
		}
		if r.URL.Query().Get("reverse") == "true" {
			slices.Reverse(items)
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("size"))
		if size < 1 || size > 100 || page < 1 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{keyDetail: "bad page"})
			return
		}
		lo, hi := min((page-1)*size, len(items)), min(page*size, len(items))
		writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(items[lo:hi]), "total": len(items), "page": page, "size": size,
			"pages": (len(items) + size - 1) / size})
	case r.Method == http.MethodPost && rest == "/conclusions/query":
		// Honcho v3.6.0 takes one observer and one observed for a search
		// and fails with 500 on anything else.
		_, one := filters["observer_id"].(string)
		_, oneObserved := filters["observed_id"].(string)
		if !one || !oneObserved {
			writeJSON(w, http.StatusInternalServerError, map[string]string{keyDetail: "An unexpected error occurred"})
			return
		}
		q, _ := body["query"].(string)
		var items []Conclusion
		for _, c := range h.Items {
			if h.match(c, filters) && strings.Contains(strings.ToLower(c.Content), strings.ToLower(q)) {
				items = append(items, c)
			}
		}
		writeJSON(w, http.StatusOK, nonNil(items))
	case r.Method == http.MethodPost && rest == "/conclusions":
		list, _ := body["conclusions"].([]any)
		var out []Conclusion
		for _, x := range list {
			m, _ := x.(map[string]any)
			s := func(k string) string { v, _ := m[k].(string); return v }
			h.next++
			c := Conclusion{ID: fmt.Sprintf("c%d", h.next), Content: s("content"), ObserverID: s("observer_id"), ObservedID: s("observed_id"),
				Level: "explicit", CreatedAt: time.Date(2026, 11, 1, 0, 0, h.next, 0, time.UTC)}
			h.Items = append([]Conclusion{c}, h.Items...)
			out = append(out, c)
		}
		writeJSON(w, http.StatusCreated, out)
	case strings.HasPrefix(rest, "/conclusions/"):
		id := strings.TrimPrefix(rest, "/conclusions/")
		i := slices.IndexFunc(h.Items, func(c Conclusion) bool { return c.ID == id })
		if i < 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{keyDetail: "not found"})
			return
		}
		if r.Method == http.MethodDelete {
			h.Items = slices.Delete(h.Items, i, i+1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, http.StatusOK, h.Items[i])
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "/peers/") && strings.HasSuffix(rest, "/card"):
		if h.MissingPeers {
			writeJSON(w, http.StatusNotFound, map[string]string{keyDetail: "peer not found"})
			return
		}
		peer := strings.TrimSuffix(strings.TrimPrefix(rest, "/peers/"), "/card")
		key := peer
		if t := r.URL.Query().Get("target"); t != "" {
			key += ">" + t
		}
		var card any
		if c, ok := h.Cards[key]; ok {
			card = c
		}
		writeJSON(w, http.StatusOK, map[string]any{"peer_card": card})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{keyDetail: notFound})
	}
}

func nonNil(c []Conclusion) []Conclusion {
	if c == nil {
		return []Conclusion{}
	}
	return c
}
