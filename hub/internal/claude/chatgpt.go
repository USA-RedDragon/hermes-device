package claude

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// ChatGPT plan usage comes from Hermes itself, not from the plugin: Hermes
// signs in to a ChatGPT plan with its openai-codex provider and reads the
// plan's usage windows from ChatGPT. Its model options carry what it last
// read for each signed-in provider.
const (
	// ChatGPTProvider is the Hermes provider of a ChatGPT plan.
	ChatGPTProvider = "openai-codex"
	// ChatGPTAccount is the account ID the hub gives the ChatGPT plan.
	ChatGPTAccount = ChatGPTProvider
	// ChatGPTLabel names the ChatGPT plan's account.
	ChatGPTLabel = "ChatGPT"
	// ModelOptionsPath is the dashboard's twin of the gateway's
	// model.options.
	ModelOptionsPath = "/api/model/options"
	// maxOptions bounds the model options answer, which lists every model.
	maxOptions = 16 << 20
)

// Answers when Hermes has no usage to show.
const (
	chatGPTNotRead = "Hermes has not read it yet. Try again in a minute."
	chatGPTPooled  = "This Hermes has more than one ChatGPT login, and the hub shows usage for one login only."
)

// ProviderWindow is one usage window of a model options provider row.
type ProviderWindow struct {
	Label       string    `json:"label"`
	UsedPercent *float64  `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at"`
}

// ProviderUsage is a provider row's usage: Windows for one login, or
// Accounts for a pool of several logins.
type ProviderUsage struct {
	Windows  []ProviderWindow  `json:"windows"`
	Accounts []json.RawMessage `json:"accounts"`
}

// ProviderRow is one provider row of the model options. Usage is nil when
// Hermes has no fresh read.
type ProviderRow struct {
	Slug          string         `json:"slug"`
	Authenticated *bool          `json:"authenticated"`
	Usage         *ProviderUsage `json:"usage"`
}

// ModelOptions is the dashboard's GET /api/model/options answer.
type ModelOptions struct {
	Providers []ProviderRow `json:"providers"`
}

// ModelOptions reads the model options of the dashboard.
func (c *Client) ModelOptions(ctx context.Context) (ModelOptions, error) {
	var out ModelOptions
	return out, c.do(ctx, false, http.MethodGet, ModelOptionsPath, maxOptions, nil, &out)
}

type chatGPTEntry struct {
	at      time.Time
	present bool
	usage   Usage
}

// chatGPTWindow maps a Hermes window label to a window kind and label as
// the plugin names Claude's.
func chatGPTWindow(w ProviderWindow) Window {
	out := Window{Kind: strings.ToLower(w.Label), Label: w.Label, UsedPercent: w.UsedPercent, ResetsAt: w.ResetsAt}
	switch w.Label {
	case "Session":
		out.Kind, out.Label = hub.ClaudeWindowSession, "Current session"
	case "Weekly":
		out.Kind, out.Label = hub.ClaudeWindowWeekly, "Current week"
	}
	return out
}

func codexRow(o ModelOptions) *ProviderRow {
	for i, p := range o.Providers {
		if p.Slug == ChatGPTProvider {
			if p.Authenticated != nil && !*p.Authenticated {
				return nil
			}
			return &o.Providers[i]
		}
	}
	return nil
}

func hasUsage(r *ProviderRow) bool {
	return r.Usage != nil && (len(r.Usage.Windows) > 0 || len(r.Usage.Accounts) > 0)
}

// chatGPT returns a backend's ChatGPT plan from the cache or the
// dashboard. Hermes answers from its own cache and reads ChatGPT in the
// background when that is old, so an answer without usage is asked once
// more after UsageRetry.
func (s *Service) chatGPT(ctx context.Context, backendID string, c *Client, fresh bool) (chatGPTEntry, error) {
	now := s.o.Now()
	s.mu.Lock()
	e, ok := s.chatgpt[backendID]
	s.mu.Unlock()
	if ok && (now.Sub(e.at) < freshAfter || (!fresh && now.Sub(e.at) < cacheTTL)) {
		return e, nil
	}
	cctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
	defer cancel()
	o, err := c.ModelOptions(cctx)
	var pe *Error
	if errors.As(err, &pe) && (pe.Status == http.StatusNotFound || pe.Status == http.StatusMethodNotAllowed) {
		err, o = nil, ModelOptions{}
	}
	if err != nil {
		return e, err
	}
	row := codexRow(o)
	if row != nil && !hasUsage(row) {
		select {
		case <-cctx.Done():
		case <-time.After(s.o.UsageRetry):
			if again, err := c.ModelOptions(cctx); err == nil {
				if r := codexRow(again); r != nil {
					row = r
				}
			}
		}
	}
	e = chatGPTEntry{at: now, present: row != nil}
	switch {
	case row == nil:
	case !hasUsage(row):
		e.usage.UnavailableReason = chatGPTNotRead
	case len(row.Usage.Windows) == 0:
		e.usage.UnavailableReason = chatGPTPooled
	default:
		for _, w := range row.Usage.Windows {
			e.usage.Windows = append(e.usage.Windows, chatGPTWindow(w))
		}
	}
	s.mu.Lock()
	s.chatgpt[backendID] = e
	s.mu.Unlock()
	return e, nil
}

// chatGPTAccount is the hub's account of a ChatGPT plan.
func (s *Service) chatGPTAccount(e chatGPTEntry, usage bool) hub.ClaudeAccount {
	a := hub.ClaudeAccount{ID: ChatGPTAccount, Label: ChatGPTLabel, Provider: ChatGPTProvider, SignedIn: true, NoSignIn: true}
	if usage {
		u := s.convertUsage(e.usage)
		a.Usage = &u
	}
	return a
}
