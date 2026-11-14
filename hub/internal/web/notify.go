package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/notify"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/webpush"
)

// Notifier is the part of notify.Notifier the web UI uses.
type Notifier interface {
	PushKey() string
	Test(ctx context.Context, user string) (notify.TestResult, error)
}

// maxPushSubscriptions is how many browsers one person may subscribe.
const maxPushSubscriptions = 10

// maxEndpoint is the longest push endpoint the hub takes.
const maxEndpoint = 2048

func (s *Server) notifyRoutes(mux *http.ServeMux) {
	mux.Handle("POST /api/presence", s.api(s.handlePresence))
	mux.Handle("POST /api/view-report", s.viewReportRoute())
	mux.Handle("POST /api/sessions/{backend}/{session}/prompts/{prompt}/answer", s.api(s.handleAnswer))
	if s.notices == nil {
		return
	}
	mux.Handle("GET /api/notify", s.api(s.handleNotify))
	mux.Handle("POST /api/notify/settings", s.api(s.handleNotifySettings))
	mux.Handle("POST /api/notify/push", s.api(s.handlePushSubscribe))
	mux.Handle("POST /api/notify/push/remove", s.api(s.handlePushRemove))
	mux.Handle("POST /api/notify/push/check", s.api(s.handlePushCheck))
	mux.Handle("POST /api/notify/test", s.api(s.handleNotifyTest))
}

type notifyJSON struct {
	Settings      hub.NotifySettings     `json:"settings"`
	PushKey       string                 `json:"push_key"`
	Subscriptions []hub.PushSubscription `json:"subscriptions"`
	Telegram      bool                   `json:"telegram"`
	// ClaudeAdmin says the person signs Claude accounts in somewhere.
	ClaudeAdmin bool `json:"claude_admin"`
}

func (s *Server) notifyState(ctx context.Context, user string) (notifyJSON, error) {
	out := notifyJSON{Subscriptions: []hub.PushSubscription{}, ClaudeAdmin: s.isClaudeAdmin(user)}
	var err error
	if out.Settings, err = s.notices.NotifySettings(ctx, user); err != nil {
		return out, err
	}
	if s.notifier != nil {
		out.PushKey = s.notifier.PushKey()
	}
	subs, err := s.notices.PushSubscriptions(ctx, user)
	if err != nil {
		return out, err
	}
	out.Subscriptions = append(out.Subscriptions, subs...)
	ids, err := s.notices.TelegramIDs(ctx, user)
	out.Telegram = len(ids) > 0
	return out, err
}

func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	out, err := s.notifyState(r.Context(), userFrom(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleNotifySettings(w http.ResponseWriter, r *http.Request) {
	var body hub.NotifySettings
	if !decode(w, r, &body) {
		return
	}
	if err := notify.ValidSettings(body); err != nil {
		errorJSON(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.notices.SetNotifySettings(r.Context(), userFrom(r), body); err != nil {
		s.fail(w, err)
		return
	}
	s.handleNotify(w, r)
}

type pushBody struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Label string `json:"label"`
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil || s.notifier.PushKey() == "" {
		errorJSON(w, http.StatusNotFound, "Web Push is not set up on this hub")
		return
	}
	var body pushBody
	if !decode(w, r, &body) {
		return
	}
	sub := webpush.Subscription{Endpoint: body.Endpoint, P256dh: body.Keys.P256dh, Auth: body.Keys.Auth}
	if webpush.IsKeylessChromium(body.Endpoint) {
		errorJSON(w, http.StatusBadRequest, "This browser is a Chromium build without Google API keys, which cannot receive Web Push. Use Google Chrome or Firefox, or a Chromium build with Google API keys")
		return
	}
	if len(body.Endpoint) > maxEndpoint || sub.Validate() != nil {
		errorJSON(w, http.StatusBadRequest, "This browser's push service is not one the hub sends to")
		return
	}
	user := userFrom(r)
	subs, err := s.notices.PushSubscriptions(r.Context(), user)
	if err != nil {
		s.fail(w, err)
		return
	}
	known := false
	for _, ps := range subs {
		known = known || ps.Endpoint == body.Endpoint
	}
	if !known && len(subs) >= maxPushSubscriptions {
		errorJSON(w, http.StatusConflict, "You have too many devices with notifications. Remove one first")
		return
	}
	label := strings.TrimSpace(body.Label)
	if r := []rune(label); len(r) > 60 {
		label = string(r[:60])
	}
	if label == "" {
		label = "Browser"
	}
	_, err = s.notices.SavePushSubscription(r.Context(), hub.PushSubscription{User: user, Endpoint: body.Endpoint,
		P256dh: body.Keys.P256dh, Auth: body.Keys.Auth, Label: label})
	if errors.Is(err, store.ErrPushInUse) {
		errorJSON(w, http.StatusConflict, "This browser's notifications belong to someone else. Turn them off and on again")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.handleNotify(w, r)
}

func (s *Server) handlePushRemove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID       string `json:"id"`
		Endpoint string `json:"endpoint"`
	}
	if !decode(w, r, &body) {
		return
	}
	key := body.ID
	if key == "" {
		key = body.Endpoint
	}
	if key == "" {
		errorJSON(w, http.StatusBadRequest, "name the subscription")
		return
	}
	if _, err := s.notices.DeletePushSubscription(r.Context(), userFrom(r), key); err != nil {
		s.fail(w, err)
		return
	}
	s.handleNotify(w, r)
}

// handlePushCheck says whether a browser's subscription is the signed-in
// person's, so the page can drop one that another person left behind.
func (s *Server) handlePushCheck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Endpoint string `json:"endpoint"`
	}
	if !decode(w, r, &body) {
		return
	}
	subs, err := s.notices.PushSubscriptions(r.Context(), userFrom(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	mine := false
	for _, ps := range subs {
		mine = mine || (body.Endpoint != "" && ps.Endpoint == body.Endpoint)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"mine": mine})
}

func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil {
		errorJSON(w, http.StatusNotFound, "notifications are off")
		return
	}
	res, err := s.notifier.Test(r.Context(), userFrom(r))
	if errors.Is(err, notify.ErrTooSoon) {
		errorJSON(w, http.StatusTooManyRequests, "Wait a minute before the next test")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handlePresence records whether the person has a chat open in a visible
// page, so the hub does not notify them about what they are looking at.
func (s *Server) handlePresence(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Backend string `json:"backend"`
		Session string `json:"session"`
		Visible bool   `json:"visible"`
	}
	if !decode(w, r, &body) {
		return
	}
	if s.notices == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if body.Backend == "" || body.Session == "" || len(body.Session) > 200 {
		errorJSON(w, http.StatusBadRequest, "name the chat")
		return
	}
	a, ok := s.svc.(interface {
		Allowed(ctx context.Context, user, backendID string) error
	})
	if ok {
		if err := a.Allowed(r.Context(), userFrom(r), body.Backend); err != nil {
			s.fail(w, err)
			return
		}
	}
	if err := s.notices.SetPresence(r.Context(), userFrom(r), body.Backend, body.Session, body.Visible); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAnswer answers an approval with any choice it offers, or a
// question.
func (s *Server) handleAnswer(w http.ResponseWriter, r *http.Request) {
	p, ok := s.svc.(hub.Prompter)
	if !ok {
		errorJSON(w, http.StatusNotFound, "this hub cannot answer questions")
		return
	}
	var body hub.Answer
	if !decode(w, r, &body) {
		return
	}
	err := p.AnswerPrompt(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), r.PathValue("prompt"), body)
	s.done(w, err)
}
