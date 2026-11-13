package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// sessionTokens is the optional part of hub.Insights that reads the tokens
// of single sessions.
type sessionTokens interface {
	SessionTokens(ctx context.Context, user, backendID string, ids []string) (map[string]hub.Tokens, error)
}

func (s *Server) activity(w http.ResponseWriter) (hub.Activity, bool) {
	a, ok := s.svc.(hub.Activity)
	if !ok {
		errorJSON(w, http.StatusNotFound, "this hub does not show activity")
	}
	return a, ok
}

func (s *Server) activityFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hub.ErrUnknownSubagent):
		errorJSON(w, http.StatusNotFound, "That subagent is no longer running")
	case errors.Is(err, hub.ErrUnknownProcess):
		errorJSON(w, http.StatusNotFound, "That command is not one of this chat's")
	case errors.Is(err, hub.ErrSteerRejected):
		errorJSON(w, http.StatusConflict, "The subagent no longer takes messages: it is finishing or has ended")
	case errors.Is(err, hub.ErrNoToolBackground):
		errorJSON(w, http.StatusNotImplemented, "This agent server cannot move a running command to the background yet. Upgrade Talaria on the server")
	case errors.Is(err, hub.ErrNoRunningTool):
		errorJSON(w, http.StatusConflict, "The command is no longer running")
	case errors.Is(err, hub.ErrUsageUnavailable):
		errorJSON(w, http.StatusBadGateway, strings.TrimPrefix(err.Error(), hub.ErrUsageUnavailable.Error()+": "))
	default:
		s.fail(w, err)
	}
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	a, ok := s.activity(w)
	if !ok {
		return
	}
	st, err := a.Activity(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"))
	if err != nil {
		s.activityFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSubagentTail(w http.ResponseWriter, r *http.Request) {
	a, ok := s.activity(w)
	if !ok {
		return
	}
	t, err := a.SubagentTail(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), r.PathValue("subagent"))
	if err != nil {
		s.activityFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleSubagentEvents lists a subagent's steps after the seq in after: its
// text, thinking, tool calls with their results, and the messages the user
// sent it. They stay readable after the subagent ends.
func (s *Server) handleSubagentEvents(w http.ResponseWriter, r *http.Request) {
	a, ok := s.svc.(hub.SubagentTranscripts)
	if !ok {
		errorJSON(w, http.StatusNotFound, "this hub does not keep subagent steps")
		return
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			errorJSON(w, http.StatusBadRequest, "after must be a seq")
			return
		}
		after = n
	}
	out, err := a.SubagentEvents(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), r.PathValue("subagent"), after)
	if err != nil {
		s.activityFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSteerSubagent(w http.ResponseWriter, r *http.Request) {
	a, ok := s.activity(w)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	err := a.SteerSubagent(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), r.PathValue("subagent"), body.Text)
	if err != nil {
		s.activityFail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleStopSubagent(w http.ResponseWriter, r *http.Request) {
	a, ok := s.activity(w)
	if !ok {
		return
	}
	if err := a.StopSubagent(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), r.PathValue("subagent")); err != nil {
		s.activityFail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleStopProcess(w http.ResponseWriter, r *http.Request) {
	a, ok := s.activity(w)
	if !ok {
		return
	}
	if err := a.StopProcess(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), r.PathValue("process")); err != nil {
		s.activityFail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleBackgroundTool(w http.ResponseWriter, r *http.Request) {
	a, ok := s.svc.(hub.ToolMover)
	if !ok {
		errorJSON(w, http.StatusNotFound, "this hub cannot move commands to the background")
		return
	}
	err := a.BackgroundTool(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), r.PathValue("tool"))
	if err != nil {
		s.activityFail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handlePauseSubagents(w http.ResponseWriter, r *http.Request) {
	a, ok := s.activity(w)
	if !ok {
		return
	}
	var body struct {
		Paused bool `json:"paused"`
	}
	if !decode(w, r, &body) {
		return
	}
	paused, err := a.PauseSubagents(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), body.Paused)
	if err != nil {
		s.activityFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"paused": paused})
}

func (s *Server) handleBackground(w http.ResponseWriter, r *http.Request) {
	a, ok := s.activity(w)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !s.backgroundHere(w, r) {
		return
	}
	id, err := a.Background(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), body.Text)
	if err != nil {
		s.activityFail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"task_id": id})
}

// handleBackgroundTokens reads the tokens of finished /bg prompts: the
// task query values, each a bg_ task ID.
func (s *Server) handleBackgroundTokens(w http.ResponseWriter, r *http.Request) {
	user, backendID := userFrom(r), r.PathValue("backend")
	if a, ok := s.svc.(interface {
		Allowed(ctx context.Context, user, backendID string) error
	}); ok {
		if err := a.Allowed(r.Context(), user, backendID); err != nil {
			s.fail(w, err)
			return
		}
	}
	var ids []string
	for _, id := range r.URL.Query()["task"] {
		if hub.IsBackground(id) && len(id) <= 64 {
			ids = append(ids, id)
		}
	}
	out := map[string]hub.Tokens{}
	st, ok := s.insights.(sessionTokens)
	if !ok || len(ids) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	out, err := st.SessionTokens(r.Context(), user, backendID, ids)
	if err != nil {
		s.activityFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// backgroundHere refuses a /bg prompt in a chat on a computer, and in one
// that was taken off its computer. Hermes runs a background prompt as a
// session of its own with no lineage to the chat, so the device plugin
// would run its commands where it runs new chats (the sandbox or the
// Hermes server), not on the chat's computer, and would run them for a
// chat that should run none.
func (s *Server) backgroundHere(w http.ResponseWriter, r *http.Request) bool {
	if s.computers == nil {
		return true
	}
	p, err := s.computers.Place(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"))
	if err != nil {
		s.fail(w, err)
		return false
	}
	switch p.Kind {
	case "", hub.PlaceSandbox, hub.PlaceServer:
		return true
	case hub.PlaceComputer:
		errorJSON(w, http.StatusConflict, "/bg does not work in a chat on a computer: the agent would run it in the sandbox or on the server, not on "+
			placeName(p))
	default:
		errorJSON(w, http.StatusConflict, "/bg does not work in this chat: it runs no commands until you move it to a computer")
	}
	return false
}

func placeName(p hub.Place) string {
	if p.Name != "" {
		return p.Name
	}
	return "the computer"
}
