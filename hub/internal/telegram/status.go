package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// sessionStatus is what the bot last heard about an attached session's
// model and context window, for /status.
type sessionStatus struct {
	mu     sync.Mutex
	model  string
	used   int64
	window int64
}

type usageFields struct {
	Model       string `json:"model"`
	ContextUsed *int64 `json:"context_used"`
	ContextMax  *int64 `json:"context_max"`
}

type infoFields struct {
	Model         string          `json:"model"`
	ContextLength *int64          `json:"context_length"`
	Usage         json.RawMessage `json:"usage"`
	Operation     string          `json:"operation"`
}

// track reads the model and context figures from one session event.
func (s *sessionStatus) track(ev hub.Event) {
	var p infoFields
	switch ev.Kind {
	case hub.KindSnapshot:
		var snap hub.Snapshot
		if json.Unmarshal(ev.Data, &snap) != nil {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.model, s.used, s.window = snap.Info.Model, 0, snap.Info.ContextLength
		s.usage(snap.Info.Usage)
		return
	case "session.info", "session.usage", hub.KindMessageComplete:
	case "session.updated":
		if json.Unmarshal(ev.Data, &p) != nil || p.Operation != gateway.OpModel || p.Model == "" {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if p.Model != s.model {
			s.model, s.used, s.window = p.Model, 0, 0
		}
		return
	default:
		return
	}
	if json.Unmarshal(ev.Data, &p) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Model != "" && p.Model != s.model {
		s.model, s.used, s.window = p.Model, 0, 0
	}
	if p.ContextLength != nil && *p.ContextLength > 0 {
		s.window = *p.ContextLength
	}
	s.usage(p.Usage)
}

// usage reads a gateway usage object. s.mu is held.
func (s *sessionStatus) usage(raw json.RawMessage) {
	var u usageFields
	if len(raw) == 0 || json.Unmarshal(raw, &u) != nil {
		return
	}
	if u.Model != "" && s.model == "" {
		s.model = u.Model
	}
	if u.ContextMax != nil && *u.ContextMax > 0 {
		s.window = *u.ContextMax
	}
	if u.ContextUsed != nil {
		s.used = *u.ContextUsed
	}
}

// line is the short status text: the model and how full its context is.
func (s *sessionStatus) line() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	model := s.model
	if model == "" {
		model = "not known yet"
	}
	fmt.Fprintf(&b, "Model: <b>%s</b>\n", escapeHTML(model))
	switch {
	case s.window > 0 && s.used > 0:
		pct := min(100, (s.used*100+s.window/2)/s.window)
		fmt.Fprintf(&b, "Context: %s / %s (%d%%)", tokens(s.used), tokens(s.window), pct)
		if pct >= 80 {
			b.WriteString(". Nearly full: Talaria will summarize older messages soon.")
		}
	case s.window > 0:
		fmt.Fprintf(&b, "Context: %s window", tokens(s.window))
	default:
		b.WriteString("Context: shows after the next reply")
	}
	return b.String()
}

// tokens writes a token count the way the web page does: 41k, 1.2M.
func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64((n+50_000)/100_000)/10, 'f', -1, 64) + "M"
	case n >= 1000:
		return strconv.FormatInt((n+500)/1000, 10) + "k"
	}
	return strconv.FormatInt(n, 10)
}

func (b *Bot) status(ctx context.Context, to dest, user string) {
	a, ok := b.attached(ctx, to, user)
	if !ok {
		return
	}
	b.sendHTML(ctx, to, a.status.line(), nil)
}
