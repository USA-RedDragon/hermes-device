package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// maxDraft bounds a draft's text.
const maxDraft = 64 << 10

func (s *Server) composeRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/sessions/{backend}/{session}/draft", s.api(s.handleDraft))
	mux.Handle("PUT /api/sessions/{backend}/{session}/draft", s.api(s.handleSetDraft))
	mux.Handle("POST /api/sessions/{backend}/{session}/pin", s.api(s.handlePin))
	mux.Handle("PUT /api/pins/order", s.api(s.handlePinOrder))
	mux.Handle("POST /api/sessions/{backend}/{session}/read", s.api(s.handleRead))
	mux.Handle("GET /api/sessions/{backend}/{session}/commands", s.api(s.handleCommands))
	mux.Handle("GET /api/sessions/{backend}/{session}/transcript", s.api(s.handleTranscript))
	mux.Handle("POST /api/sessions/{backend}/{session}/rewind", s.api(s.handleRewind))
	mux.Handle("POST /api/sessions/{backend}/{session}/branch", s.api(s.handleBranch))
}

// allowedHere fails the request unless the user may use the path's
// backend.
func (s *Server) allowedHere(w http.ResponseWriter, r *http.Request) bool {
	a, ok := s.svc.(interface {
		Allowed(ctx context.Context, user, backendID string) error
	})
	if !ok {
		return true
	}
	if err := a.Allowed(r.Context(), userFrom(r), r.PathValue("backend")); err != nil {
		s.fail(w, err)
		return false
	}
	return true
}

// chatStore returns the chat store after the access check, or answers 404
// when the hub keeps no chat state.
func (s *Server) chatStore(w http.ResponseWriter, r *http.Request) (hub.ChatStore, bool) {
	cs, ok := s.userChats(w)
	if !ok || !s.allowedHere(w, r) {
		return nil, false
	}
	return cs, true
}

// userChats is the chat store, or answers 404 when the hub keeps none. It
// only reads and writes the signed-in user's own marks.
func (s *Server) userChats(w http.ResponseWriter) (hub.ChatStore, bool) {
	if s.chats == nil {
		errorJSON(w, http.StatusNotFound, "the hub keeps no drafts or chat marks")
		return nil, false
	}
	return s.chats, true
}

type draftJSON struct {
	Text      string    `json:"text"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

func (s *Server) handleDraft(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.chatStore(w, r)
	if !ok {
		return
	}
	text, at, err := cs.Draft(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, draftJSON{Text: text, UpdatedAt: at})
}

func (s *Server) handleSetDraft(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.chatStore(w, r)
	if !ok {
		return
	}
	var body struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Text) > maxDraft {
		errorJSON(w, http.StatusRequestEntityTooLarge, "The draft is longer than 64 KB")
		return
	}
	if err := cs.SetDraft(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), body.Text); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.chatStore(w, r)
	if !ok {
		return
	}
	var body struct {
		Pinned bool `json:"pinned"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := cs.SetPinned(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), body.Pinned); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxPinOrder caps the chats one reorder names.
const maxPinOrder = 1000

func (s *Server) handlePinOrder(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.userChats(w)
	if !ok {
		return
	}
	var body struct {
		Order []struct {
			BackendID string `json:"backend_id"`
			SessionID string `json:"session_id"`
		} `json:"order"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Order) > maxPinOrder {
		errorJSON(w, http.StatusBadRequest, "too many chats")
		return
	}
	order := make([]hub.SessionRef, 0, len(body.Order))
	for _, x := range body.Order {
		if x.BackendID == "" || x.SessionID == "" {
			errorJSON(w, http.StatusBadRequest, "each chat needs backend_id and session_id")
			return
		}
		order = append(order, hub.SessionRef{BackendID: x.BackendID, SessionID: x.SessionID})
	}
	if err := cs.SetPinOrder(r.Context(), userFrom(r), order); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	cs, ok := s.chatStore(w, r)
	if !ok {
		return
	}
	var body struct {
		Unread bool      `json:"unread"`
		At     time.Time `json:"at"`
	}
	if !decode(w, r, &body) {
		return
	}
	user, backendID, sessionID := userFrom(r), r.PathValue("backend"), r.PathValue("session")
	var err error
	if body.Unread {
		err = cs.MarkUnread(r.Context(), user, backendID, sessionID)
	} else {
		err = cs.MarkRead(r.Context(), user, backendID, sessionID, body.At)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// marked adds the user's pins, unread state and what each chat waits for.
// Without a chat store, or when it fails, chats are unpinned and read.
func (s *Server) marked(ctx context.Context, user string, ss []hub.Session) []sessionJSON {
	var (
		marks map[hub.SessionRef]hub.ChatMark
		base  time.Time
	)
	if s.chats != nil {
		var err error
		if marks, base, err = s.chats.ChatMarks(ctx, user); err != nil {
			s.log.Warn("read chat marks", "error", err.Error())
			marks = nil
		}
	}
	out := make([]sessionJSON, 0, len(ss))
	for _, x := range ss {
		j := toJSON(x)
		if marks != nil {
			m := marks[hub.SessionRef{BackendID: x.BackendID, SessionID: x.ID}]
			j.Pinned, j.PinPos, j.Unread, j.Page = m.Pinned, m.PinPos, m.IsUnread(x, base), m.Page
		}
		out = append(out, j)
	}
	return out
}

// actions returns the service's history actions, or answers 404.
func (s *Server) actions(w http.ResponseWriter) (hub.ChatActions, bool) {
	a, ok := s.svc.(hub.ChatActions)
	if !ok {
		errorJSON(w, http.StatusNotFound, "not available")
	}
	return a, ok
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
	a, ok := s.actions(w)
	if !ok {
		return
	}
	cmds, err := a.Commands(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cmds)
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	a, ok := s.actions(w)
	if !ok {
		return
	}
	t, err := a.Transcript(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

type rewindJSON struct {
	promptJSON
	Removed int              `json:"removed"`
	Model   *hub.ModelChange `json:"model,omitempty"`
}

func (s *Server) handleRewind(w http.ResponseWriter, r *http.Request) {
	a, ok := s.actions(w)
	if !ok {
		return
	}
	var body struct {
		Text     string `json:"text"`
		Retry    bool   `json:"retry"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Confirm  string `json:"confirm"`
	}
	if !decode(w, r, &body) {
		return
	}
	res, err := a.Rewind(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"),
		hub.Rewind{Text: body.Text, Retry: body.Retry, Provider: body.Provider, Model: body.Model, Confirm: body.Confirm})
	switch {
	case errors.Is(err, hub.ErrNoUserMessage):
		errorJSON(w, http.StatusConflict, "This chat has no message of yours to go back to")
		return
	case err != nil:
		s.fail(w, err)
		return
	case res.Model != nil && res.Model.Status == hub.ModelConfirm:
		writeJSON(w, http.StatusOK, res.Model)
		return
	}
	writeJSON(w, http.StatusAccepted, rewindJSON{promptJSON: toPromptJSON(res.Prompt), Removed: res.Removed, Model: res.Model})
}

type branchJSON struct {
	BackendID string `json:"backend_id"`
	SessionID string `json:"session_id"`
}

// handleBranch copies a chat into a new one. On a backend with devices the
// new chat must run where its parent runs, or nowhere: Hermes records the
// parent on the new session, so the device plugin resolves it to the
// parent's binding. The hub checks that, and deletes a branch that would
// run anywhere else, so its tools never run on the Hermes server or the
// sandbox when the parent's run on a computer.
func (s *Server) handleBranch(w http.ResponseWriter, r *http.Request) {
	a, ok := s.actions(w)
	if !ok {
		return
	}
	var body struct {
		RowID *int64 `json:"row_id"`
		Title string `json:"title"`
	}
	if !decode(w, r, &body) {
		return
	}
	user, backendID, sessionID := userFrom(r), r.PathValue("backend"), r.PathValue("session")
	placed := s.computers != nil && s.deviceBackendIDs[backendID]
	var parent hub.Place
	if placed {
		var err error
		if parent, err = s.computers.Place(r.Context(), user, backendID, sessionID); err != nil {
			s.fail(w, err)
			return
		}
	}
	var row int64
	if body.RowID != nil {
		row = *body.RowID
		if row <= 0 {
			errorJSON(w, http.StatusBadRequest, "row_id must be a positive number")
			return
		}
	}
	child, err := a.Branch(r.Context(), user, backendID, sessionID, row, body.Title)
	if err != nil {
		s.fail(w, err)
		return
	}
	if placed {
		p, err := s.computers.Place(r.Context(), user, backendID, child.ID)
		if err != nil || !samePlace(parent, p) {
			s.log.Warn("deleting a branch that would run elsewhere than its parent", "backend", backendID,
				"parent", sessionID, "branch", child.ID, "parent_place", parent.Kind, "branch_place", p.Kind)
			if derr := s.svc.DeleteSession(context.WithoutCancel(r.Context()), user, backendID, child.ID); derr != nil {
				s.log.Error("delete a misplaced branch", "backend", backendID, "branch", child.ID, "error", derr.Error())
			}
			errorJSON(w, http.StatusConflict, "The copy would not run where this chat runs, so the hub removed it. Try again")
			return
		}
	}
	writeJSON(w, http.StatusCreated, branchJSON{BackendID: child.BackendID, SessionID: child.ID})
}

// samePlace reports whether a branch at child runs where its parent runs,
// or nowhere. On a computer the branch must follow a binding of its
// lineage (Inherited), which is the parent's, and not one of its own. A
// sandbox's folder follows the session, so only its device counts.
func samePlace(parent, child hub.Place) bool {
	switch {
	case child.Kind == hub.PlaceNone:
		return true
	case child.Kind != parent.Kind:
		return false
	case child.Kind == hub.PlaceComputer:
		return child.Inherited && child.DeviceID == parent.DeviceID && child.Dir == parent.Dir
	case child.Kind == hub.PlaceSandbox:
		return child.DeviceID == parent.DeviceID
	}
	return true
}

func toPromptJSON(res hub.PromptResult) promptJSON {
	out := promptJSON{ID: res.ID, Status: promptSent, Notice: res.Notice, Steered: res.Steered}
	switch {
	case res.Held:
		out.Status, out.HeldUntil = promptHeld, res.HeldUntil
	case res.Command:
		out.Status, out.Output = promptCommand, res.Output
	}
	return out
}

// unknownCommand answers a slash command Hermes does not run from the hub,
// with Hermes' hint when it gave one.
func unknownCommand(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, hub.ErrUnknownCommand) {
		return false
	}
	msg := "Hermes does not run this command from the hub"
	if hint, ok := strings.CutPrefix(err.Error(), hub.ErrUnknownCommand.Error()+": "); ok && hint != "" {
		msg += ". " + hint
	}
	errorJSON(w, http.StatusBadRequest, msg)
	return true
}
