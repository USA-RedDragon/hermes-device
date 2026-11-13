package web

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/agentcfg"
)

// ChatYolo reads and sets a chat's approval bypass. *service.Service is one.
type ChatYolo interface {
	Yolo(ctx context.Context, user, backendID, sessionID string, set *bool) (bool, error)
}

// agentRoutes serve Settings > Agent, Skills and tools and Diagnostics, and
// the quick panel in a chat. Every write, and every read beyond names and
// on or off, is for the backend's agent admins (agent.admins, or the hub's
// admins when that is empty), checked here on the server.
func (s *Server) agentRoutes(mux *http.ServeMux) {
	const base = "/api/agent/{backend}"
	admin := func(h http.HandlerFunc) http.Handler { return s.agentAdmin(h) }
	mux.Handle("GET "+base+"/summary", s.api(s.handleAgentSummary))
	mux.Handle("GET "+base+"/settings", admin(s.handleAgentSettings))
	mux.Handle("PUT "+base+"/settings", admin(s.handleSaveAgentSettings))
	mux.Handle("PUT "+base+"/soul", admin(s.handleSaveSoul))
	mux.Handle("GET "+base+"/tools", admin(s.handleToolsets))
	mux.Handle("PUT "+base+"/tools/{name}", admin(s.handleSetToolset))
	mux.Handle("GET "+base+"/tools/{name}/keys", admin(s.handleToolsetKeys))
	mux.Handle("PUT "+base+"/tools/{name}/keys", admin(s.handleSetToolsetKeys))
	mux.Handle("GET "+base+"/skills", admin(s.handleSkills))
	mux.Handle("PUT "+base+"/skills/{name}", admin(s.handleSetSkill))
	mux.Handle("DELETE "+base+"/skills/{name}", admin(s.handleRemoveSkill))
	mux.Handle("GET "+base+"/catalog", admin(s.handleSearchSkills))
	mux.Handle("GET "+base+"/catalog/check", admin(s.handleCheckSkill))
	mux.Handle("POST "+base+"/catalog/install", admin(s.handleInstallSkill))
	mux.Handle("GET "+base+"/connections", admin(s.handleConnections))
	mux.Handle("POST "+base+"/connections", admin(s.handleAddConnection))
	mux.Handle("PUT "+base+"/connections/{name}", admin(s.handleSetConnection))
	mux.Handle("DELETE "+base+"/connections/{name}", admin(s.handleRemoveConnection))
	mux.Handle("POST "+base+"/connections/{name}/test", admin(s.handleTestConnection))
	mux.Handle("GET "+base+"/diagnostics", admin(s.handleDiagnostics))
	mux.Handle("GET "+base+"/log", admin(s.handleAgentLog))
	mux.Handle("POST "+base+"/healthcheck", admin(s.handleHealthCheck))
	mux.Handle("GET /api/sessions/{backend}/{session}/yolo", s.api(s.handleYolo))
	mux.Handle("PUT /api/sessions/{backend}/{session}/yolo", s.api(s.handleYolo))
}

// agentAdminOf says whether user may change backendID's agent.
func (s *Server) agentAdminOf(ctx context.Context, user, backendID string) bool {
	if s.agent == nil {
		return false
	}
	if len(s.agentAdmins) == 0 {
		return s.isAdmin(ctx, user)
	}
	return slices.Contains(s.agentAdmins[backendID], user)
}

// agentAdminBackends are the backends of the user's that they may change
// the agent of, for /api/me.
func (s *Server) agentAdminBackends(ctx context.Context, user string) []string {
	out := []string{}
	if s.agent == nil {
		return out
	}
	bs, err := s.svc.Backends(ctx, user)
	if err != nil {
		return out
	}
	for _, b := range bs {
		if s.agentAdminOf(ctx, user, b.ID) {
			out = append(out, b.ID)
		}
	}
	return out
}

func (s *Server) agentAdmin(h http.HandlerFunc) http.Handler {
	return s.api(func(w http.ResponseWriter, r *http.Request) {
		if s.agent == nil {
			errorJSON(w, http.StatusNotFound, "This hub cannot change the agent")
			return
		}
		if !s.agentAdminOf(r.Context(), userFrom(r), r.PathValue("backend")) {
			errorJSON(w, http.StatusForbidden, "Only an admin may change the agent")
			return
		}
		h(w, r)
	})
}

func (s *Server) agentFail(w http.ResponseWriter, err error) {
	var ae *agentcfg.Error
	if errors.As(err, &ae) {
		errorJSON(w, ae.Status, ae.Message)
		return
	}
	s.fail(w, err)
}

func (s *Server) agentJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		s.agentFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// live is the answer of a change: whether open chats get it on their next
// message.
func (s *Server) live(w http.ResponseWriter, applied bool, err error) {
	s.agentJSON(w, map[string]bool{"ok": true, "applied_live": applied}, err)
}

// summaryItem is a name and whether it is on: all a member sees.
type summaryItem struct {
	Name    string `json:"name"`
	Label   string `json:"label,omitempty"`
	Enabled bool   `json:"enabled"`
	Always  bool   `json:"always,omitempty"`
}

// handleAgentSummary answers the quick panel: for every user of the
// backend the approvals mode and the names of what is on; admins also get
// descriptions, the off ones and the locks.
func (s *Server) handleAgentSummary(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		errorJSON(w, http.StatusNotFound, "This hub cannot show the agent's tools")
		return
	}
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	if _, err := s.agent.Requester(ctx, user, backend); err != nil {
		s.agentFail(w, err)
		return
	}
	admin := s.agentAdminOf(ctx, user, backend)
	out := map[string]any{"admin": admin}
	st, err := s.agent.AgentSettings(ctx, user, backend)
	if err != nil {
		s.agentFail(w, err)
		return
	}
	out["approvals_mode"], out["live"] = st.ApprovalsMode, st.Live
	tools, err := s.agent.Toolsets(ctx, user, backend)
	if err != nil {
		s.agentFail(w, err)
		return
	}
	skills, err := s.agent.Skills(ctx, user, backend)
	if err != nil {
		s.agentFail(w, err)
		return
	}
	conns, err := s.agent.Connections(ctx, user, backend)
	if err != nil {
		s.agentFail(w, err)
		return
	}
	if admin {
		out["locked"], out["tools"], out["skills"], out["connections"] = st.Locked, tools, skills, conns
		writeJSON(w, http.StatusOK, out)
		return
	}
	ts, ss, cs := []summaryItem{}, []summaryItem{}, []summaryItem{}
	for _, t := range tools {
		if t.Enabled {
			ts = append(ts, summaryItem{Name: t.Name, Label: t.Label, Enabled: true, Always: t.Always})
		}
	}
	for _, k := range skills {
		if k.Enabled {
			ss = append(ss, summaryItem{Name: k.Name, Enabled: true})
		}
	}
	for _, c := range conns {
		if c.Enabled {
			cs = append(cs, summaryItem{Name: c.Name, Enabled: true})
		}
	}
	out["tools"], out["skills"], out["connections"] = ts, ss, cs
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAgentSettings(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	st, err := s.agent.AgentSettings(ctx, user, backend)
	s.agentJSON(w, st, err)
}

func (s *Server) handleSaveAgentSettings(w http.ResponseWriter, r *http.Request) {
	var u agentcfg.SettingsUpdate
	if !decode(w, r, &u) {
		return
	}
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	s.agentJSON(w, map[string]bool{"ok": true}, s.agent.SaveSettings(ctx, user, backend, u))
}

func (s *Server) handleSaveSoul(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Content string `json:"content"`
	}
	if !decode(w, r, &b) {
		return
	}
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	applied, err := s.agent.SaveSoul(ctx, user, backend, b.Content)
	s.live(w, applied, err)
}

func (s *Server) handleToolsets(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	v, err := s.agent.Toolsets(ctx, user, backend)
	s.agentJSON(w, v, err)
}

type onBody struct {
	Enabled bool `json:"enabled"`
}

func (s *Server) handleSetToolset(w http.ResponseWriter, r *http.Request) {
	var b onBody
	if !decode(w, r, &b) {
		return
	}
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	applied, err := s.agent.SetToolset(ctx, user, backend, r.PathValue("name"), b.Enabled)
	s.live(w, applied, err)
}

func (s *Server) handleToolsetKeys(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	v, err := s.agent.ToolsetKeys(ctx, user, backend, r.PathValue("name"))
	s.agentJSON(w, v, err)
}

// handleSetToolsetKeys saves a tool's keys. The body is never logged.
func (s *Server) handleSetToolsetKeys(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Env map[string]string `json:"env"`
	}
	if !decode(w, r, &b) {
		return
	}
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	applied, err := s.agent.SetToolsetKeys(ctx, user, backend, r.PathValue("name"), b.Env)
	s.live(w, applied, err)
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	v, err := s.agent.Skills(ctx, user, backend)
	s.agentJSON(w, v, err)
}

func (s *Server) handleSetSkill(w http.ResponseWriter, r *http.Request) {
	var b onBody
	if !decode(w, r, &b) {
		return
	}
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	applied, err := s.agent.SetSkill(ctx, user, backend, r.PathValue("name"), b.Enabled)
	s.live(w, applied, err)
}

func (s *Server) handleRemoveSkill(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	v, err := s.agent.RemoveSkill(ctx, user, backend, r.PathValue("name"))
	s.agentJSON(w, v, err)
}

func (s *Server) handleSearchSkills(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	v, err := s.agent.SearchSkills(ctx, user, backend, r.URL.Query().Get("q"))
	s.agentJSON(w, v, err)
}

func (s *Server) handleCheckSkill(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	v, err := s.agent.CheckSkill(ctx, user, backend, r.URL.Query().Get("identifier"))
	s.agentJSON(w, v, err)
}

func (s *Server) handleInstallSkill(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Identifier string `json:"identifier"`
	}
	if !decode(w, r, &b) {
		return
	}
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	v, err := s.agent.InstallSkill(ctx, user, backend, b.Identifier)
	s.agentJSON(w, v, err)
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	v, err := s.agent.Connections(ctx, user, backend)
	s.agentJSON(w, v, err)
}

// handleAddConnection adds an MCP server. The body may hold a token and is
// never logged.
func (s *Server) handleAddConnection(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name  string `json:"name"`
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if !decode(w, r, &b) {
		return
	}
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	applied, err := s.agent.AddConnection(ctx, user, backend, b.Name, b.URL, b.Token)
	s.live(w, applied, err)
}

func (s *Server) handleSetConnection(w http.ResponseWriter, r *http.Request) {
	var b onBody
	if !decode(w, r, &b) {
		return
	}
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	applied, err := s.agent.SetConnection(ctx, user, backend, r.PathValue("name"), b.Enabled)
	s.live(w, applied, err)
}

func (s *Server) handleRemoveConnection(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	applied, err := s.agent.RemoveConnection(ctx, user, backend, r.PathValue("name"))
	s.live(w, applied, err)
}

func (s *Server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	v, err := s.agent.TestConnection(ctx, user, backend, r.PathValue("name"))
	s.agentJSON(w, v, err)
}

const statusBad = "bad"

// part is one row of Settings, Diagnostics.
type part struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, bad (statusBad), off
	Text   string `json:"text"`
}

func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	parts := []part{{Name: "Hub", Status: "ok", Text: "Version " + orUnknown(s.version) + ", running " + since(s.started)}}
	st, err := s.agent.AgentStatus(ctx, user, backend)
	if err != nil {
		s.agentFail(w, err)
		return
	}
	agent := part{Name: "Agent", Status: "ok", Text: "Answering"}
	if !st.Answering {
		agent.Status, agent.Text = statusBad, "Not answering. It may be restarting. The hub holds new messages for a while."
	} else {
		if st.Version != "" {
			agent.Text += ". Release " + st.Version
		}
		if st.MemoryMB > 0 {
			agent.Text += ". Using " + strconv.FormatInt(st.MemoryMB, 10) + " MB of memory"
		}
	}
	parts = append(parts, agent)
	if s.sandboxes != nil {
		for _, sb := range s.sandboxes(ctx, user) {
			if sb.BackendID != backend {
				continue
			}
			p := part{Name: sb.Name, Status: "warn", Text: "Not checked"}
			if sb.Online != nil && *sb.Online {
				p.Status, p.Text = "ok", "Connected"
			} else if sb.Online != nil {
				p.Status, p.Text = statusBad, "Not connected"
			}
			parts = append(parts, p)
		}
	}
	if s.computers != nil {
		if cs, err := s.computers.Computers(ctx, user); err == nil {
			on, off := 0, 0
			for _, c := range cs {
				if c.BackendID != backend {
					continue
				}
				if c.Online {
					on++
				} else {
					off++
				}
			}
			p := part{Name: "Your computers", Status: "ok", Text: strconv.Itoa(on) + " connected"}
			if off > 0 {
				p.Text += ", " + strconv.Itoa(off) + " offline"
				if on == 0 {
					p.Status = "warn"
				}
			}
			if on+off == 0 {
				p.Status, p.Text = "off", "None paired yet"
			}
			parts = append(parts, p)
		}
	}
	tg := part{Name: "Telegram", Status: "off", Text: "Off"}
	if s.telegram != nil {
		ts := s.telegram.TelegramStatus(ctx)
		switch {
		case ts.Running:
			tg.Status, tg.Text = "ok", "Running"
			if ts.Bot != "" {
				tg.Text += " as " + ts.Bot
			}
		case ts.Error != "":
			tg.Status, tg.Text = statusBad, ts.Error
		}
	}
	parts = append(parts, tg)
	writeJSON(w, http.StatusOK, map[string]any{"parts": parts})
}

func orUnknown(v string) string {
	if v == "" {
		return "unknown"
	}
	return v
}

// since says how long ago t was, in words.
func since(t time.Time) string {
	if t.IsZero() {
		return "for a moment"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return strconv.Itoa(max(1, int(d.Minutes()))) + " min"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h"
	}
	return strconv.Itoa(int(d.Hours()/24)) + " days"
}

func (s *Server) handleAgentLog(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	q := r.URL.Query()
	n, _ := strconv.Atoi(q.Get("lines"))
	lines, err := s.agent.Log(ctx, user, backend, q.Get("level"), q.Get("search"), n)
	s.agentJSON(w, map[string]any{"lines": lines}, err)
}

func (s *Server) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	ctx, user, backend := r.Context(), userFrom(r), r.PathValue("backend")
	v, err := s.agent.HealthCheck(ctx, user, backend)
	s.agentJSON(w, v, err)
}

func (s *Server) handleYolo(w http.ResponseWriter, r *http.Request) {
	y, ok := s.svc.(ChatYolo)
	if !ok {
		errorJSON(w, http.StatusNotFound, "This hub cannot change how this chat asks")
		return
	}
	var set *bool
	if r.Method == http.MethodPut {
		var b onBody
		if !decode(w, r, &b) {
			return
		}
		set = &b.Enabled
	}
	on, err := y.Yolo(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), set)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": on})
}
