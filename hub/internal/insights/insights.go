// Package insights serves the hub's chat search and its Usage page: the
// hermes-device plugin's views of each backend's session store, what the
// hub saw in usage events, Honcho's backlog and the local GPU's time from
// the Ollama proxy's metrics.
package insights

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/device"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const (
	// searchChats is how many chats each backend returns for a search.
	searchChats = 20
	// MaxQuery bounds a search, as the plugin does.
	MaxQuery = 200
	// MaxDays bounds the Usage page's window, as the plugin does.
	MaxDays = 90
	// userSearches is how many searches of one user run at once.
	userSearches = 2
)

// Access says which backends a user may use. *service.Service is one.
type Access interface {
	Allowed(ctx context.Context, user, backendID string) error
	Backends(ctx context.Context, user string) ([]hub.Backend, error)
}

// Observer reports what the hub saw in a backend's usage events.
// *service.Service is one.
type Observer interface {
	Observed(ctx context.Context, user, backendID string) (hub.Observed, time.Time, error)
}

// Backlogs reads Honcho's backlog of a backend. *memory.Service is one.
type Backlogs interface {
	Backlog(ctx context.Context, user, backendID string) (hub.Backlog, error)
}

// Options are the dependencies of a Service.
type Options struct {
	Access Access
	// Requesters reach each backend's dashboard with the hub's session.
	Requesters map[string]device.Requester
	Observer   Observer
	// Backlogs is nil when no backend has Honcho.
	Backlogs Backlogs
	// GPU reads the local GPU's time. Nil: the Usage page has none.
	GPU *GPU
	// Timeout bounds each backend's answer. Default 20s.
	Timeout time.Duration
	Logger  *slog.Logger
}

// Service implements hub.Insights.
type Service struct {
	opts    Options
	plugins map[string]*device.Plugin

	mu        sync.Mutex
	searching map[string]int
}

var _ hub.Insights = (*Service)(nil)

// New returns a Service.
func New(opts Options) *Service {
	if opts.Timeout == 0 {
		opts.Timeout = 20 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Service{opts: opts, plugins: map[string]*device.Plugin{}, searching: map[string]int{}}
	for id, r := range opts.Requesters {
		s.plugins[id] = device.NewPlugin(r)
	}
	return s
}

// unixTime is a Unix time in seconds, or the zero time for none.
func unixTime(f float64) time.Time {
	if f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) || f > 1e11 {
		return time.Time{}
	}
	return time.UnixMilli(int64(f * 1000)).UTC()
}

// pluginReason says why a backend's plugin could not answer, for the user.
func pluginReason(err error) string {
	var pe *device.PluginError
	switch {
	case errors.As(err, &pe) && pe.Code == "busy":
		return "Hermes is busy with other searches. Try again in a moment"
	case errors.As(err, &pe) && pe.Code == "store_unavailable":
		return "Hermes could not open its chat history"
	case errors.As(err, &pe) && (pe.Status == http.StatusNotFound || pe.Status == http.StatusMethodNotAllowed):
		return "The Hermes device plugin is too old for this. Update hermes-device on the Hermes server"
	case errors.As(err, &pe) && pe.Message != "":
		return pe.Message
	case errors.Is(err, context.DeadlineExceeded):
		return "Hermes took too long to answer"
	}
	return "Could not reach Hermes"
}

// Search searches the messages of user's chats on each backend they may
// use. A backend that cannot search is reported in Errors by name. See
// hub.Insights.
func (s *Service) Search(ctx context.Context, user, query string) (hub.SearchReport, error) {
	q := strings.TrimSpace(query)
	if q == "" || len([]rune(q)) > MaxQuery {
		return hub.SearchReport{}, fmt.Errorf("%w: a search is 1 to %d characters", hub.ErrBadQuery, MaxQuery)
	}
	bs, err := s.opts.Access.Backends(ctx, user)
	if err != nil {
		return hub.SearchReport{}, err
	}
	s.mu.Lock()
	if s.searching[user] >= userSearches {
		s.mu.Unlock()
		return hub.SearchReport{}, hub.ErrSearchBusy
	}
	s.searching[user]++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.searching[user]--; s.searching[user] <= 0 {
			delete(s.searching, user)
		}
		s.mu.Unlock()
	}()
	results := make([][]hub.SearchResult, len(bs))
	errs := make([]string, len(bs))
	var wg sync.WaitGroup
	for i, b := range bs {
		p := s.plugins[b.ID]
		if p == nil {
			continue
		}
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
			defer cancel()
			chats, err := p.Search(cctx, q, searchChats)
			var pe *device.PluginError
			if errors.As(err, &pe) && pe.Code == "bad_query" {
				errs[i] = pe.Message
				return
			}
			if err != nil {
				s.opts.Logger.Info("search failed", "backend", b.ID, "error", err.Error())
				errs[i] = pluginReason(err)
				return
			}
			for _, c := range chats {
				r := hub.SearchResult{BackendID: b.ID, SessionID: c.SessionID, Title: c.Title,
					LastActive: unixTime(c.LastActive), Archived: c.Archived, Hits: make([]hub.SearchHit, 0, len(c.Hits))}
				for _, h := range c.Hits {
					r.Hits = append(r.Hits, hub.SearchHit{RowID: h.RowID, SessionID: h.SessionID, Role: h.Role,
						Snippet: h.Snippet, At: unixTime(h.Timestamp)})
				}
				results[i] = append(results[i], r)
			}
		})
	}
	wg.Wait()
	out := hub.SearchReport{Results: []hub.SearchResult{}}
	for i, b := range bs {
		out.Results = append(out.Results, results[i]...)
		if errs[i] != "" {
			if out.Errors == nil {
				out.Errors = map[string]string{}
			}
			out.Errors[b.Name] = errs[i]
		}
	}
	return out, nil
}

// Usage reports one backend's token use over the last days days, with
// what the hub saw in its usage events, Honcho's backlog and the local
// GPU's time. See hub.Insights.
func (s *Service) Usage(ctx context.Context, user, backendID string, days int) (hub.UsageReport, error) {
	if err := s.opts.Access.Allowed(ctx, user, backendID); err != nil {
		return hub.UsageReport{}, err
	}
	days = max(1, min(days, MaxDays))
	out := hub.UsageReport{BackendID: backendID, Days: days, Sessions: []hub.UsageSession{}, Models: []hub.UsageModel{}}
	var wg sync.WaitGroup
	wg.Go(func() { s.tokens(ctx, backendID, days, &out) })
	if s.opts.Backlogs != nil {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
			defer cancel()
			b, err := s.opts.Backlogs.Backlog(cctx, user, backendID)
			var me *hub.MemoryError
			switch {
			case err == nil:
				out.Backlog = &b
			case errors.Is(err, hub.ErrNoMemory):
			case errors.As(err, &me):
				out.BacklogError = me.Message
			default:
				out.BacklogError = "Could not reach Honcho"
			}
		})
	}
	if s.opts.GPU != nil {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
			defer cancel()
			g, err := s.opts.GPU.Windows(cctx)
			if err != nil {
				s.opts.Logger.Info("read GPU time", "error", err.Error())
				out.GPUError = "Could not read the GPU's metrics"
				return
			}
			out.GPU = g
		})
	}
	if s.opts.Observer != nil {
		o, since, err := s.opts.Observer.Observed(ctx, user, backendID)
		if err != nil {
			wg.Wait()
			return hub.UsageReport{}, err
		}
		out.Observed, out.ObservedSince = o, since
	}
	wg.Wait()
	return out, nil
}

func (s *Service) tokens(ctx context.Context, backendID string, days int, out *hub.UsageReport) {
	p := s.plugins[backendID]
	if p == nil {
		out.TokensError = "This Hermes has no usage records the hub can read"
		return
	}
	cctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	u, err := p.Usage(cctx, days)
	if err != nil {
		s.opts.Logger.Info("read usage", "backend", backendID, "error", err.Error())
		out.TokensError = pluginReason(err)
		return
	}
	out.Since, out.Truncated = unixTime(u.Since), u.Truncated
	for _, x := range u.Sessions {
		out.Sessions = append(out.Sessions, hub.UsageSession{SessionID: x.SessionID, ChatID: x.ChatID, Title: x.Title,
			Source: x.Source, Model: x.Model, Provider: x.Provider, LastActive: unixTime(x.LastActive), Tokens: tokens(x.Counters)})
	}
	for _, x := range u.Models {
		out.Models = append(out.Models, hub.UsageModel{Model: x.Model, Provider: x.Provider, Tokens: tokens(x.Counters)})
	}
}

func tokens(c device.Counters) hub.Tokens {
	cost := c.CostUSD
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		cost = 0
	}
	return hub.Tokens{Input: c.Input, Output: c.Output, CacheRead: c.CacheRead, CacheWrite: c.CacheWrite,
		Reasoning: c.Reasoning, Calls: c.Calls, CostUSD: cost}
}
