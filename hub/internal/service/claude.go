package service

import (
	"encoding/json"
	"strings"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// TurnError is a turn of a chat that ended with an error, as the hub saw
// it: the error's text and the provider the chat used.
type TurnError struct {
	BackendID, SessionID, Title, Provider, Text string
}

// SetTurnErrors makes the service hand each failed turn of a subscribed
// session to f, on a goroutine of its own.
func (s *Service) SetTurnErrors(f func(TurnError)) { s.turnErrors.Store(&f) }

// Announce sends a hub event to the viewers of a session that this
// replica follows. It reports false when it follows none such.
func (s *Service) Announce(backendID, sessionID, kind string, data any) bool {
	b := s.backends[backendID]
	if b == nil {
		return false
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return false
	}
	b.mu.Lock()
	ls := b.sessions[sessionID]
	b.mu.Unlock()
	if ls == nil {
		return false
	}
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.retired {
		return false
	}
	ls.emit(kind, raw)
	return true
}

// turnFailed reports a failed turn. ls.mu is held.
func (ls *live) turnFailed(texts ...string) {
	f := ls.b.svc.turnErrors.Load()
	if f == nil {
		return
	}
	e := TurnError{BackendID: ls.b.cfg.ID, SessionID: ls.id, Title: ls.title, Provider: ls.provider,
		Text: strings.Join(texts, "\n")}
	go (*f)(e)
}

// trackProvider records the provider a session uses from a frame that
// names it. ls.mu is held.
func (ls *live) trackProvider(ev gateway.Event) {
	switch ev.Type {
	case gateway.InfoEvent:
		var i gateway.SnapshotInfo
		if json.Unmarshal(ev.Payload, &i) == nil && i.Provider != nil && *i.Provider != "" {
			ls.provider = *i.Provider
		}
	case gateway.UpdatedEvent, gateway.UsageEvent:
		var p struct {
			Provider string `json:"provider"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil && p.Provider != "" {
			ls.provider = p.Provider
		}
	}
}

// failedTurnText is the error text of a frame that ends a turn with an
// error, and whether it is one.
func failedTurnText(ev gateway.Event) (string, bool) {
	switch ev.Type {
	case hub.KindMessageComplete:
		var p struct {
			Status        string `json:"status"`
			Text          any    `json:"text"`
			Error         string `json:"error"`
			FailureReason string `json:"failure_reason"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || (p.Status != "error" && p.Status != "failed") {
			return "", false
		}
		text, _ := p.Text.(string)
		return strings.Join([]string{p.Error, p.FailureReason, text}, "\n"), true
	case hub.KindError:
		var p struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || p.Message == "" {
			return "", false
		}
		return p.Message, true
	}
	return "", false
}
