// Package subagents serves the Subagents setting of chats on backends
// whose Hermes runs the subagent-routes plugin: which model routes a
// chat's subagents may use.
package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// PluginBase is where the Hermes dashboard mounts the subagent-routes
// plugin's routes.
const PluginBase = "/api/plugins/subagent-routes"

const maxBody = 1 << 20

// Access says whether a user may use a backend. *service.Service is one.
type Access interface {
	Allowed(ctx context.Context, user, backendID string) error
}

// Options are the dependencies of a Service.
type Options struct {
	Access Access
	// Requesters reach each backend's dashboard with the hub's session.
	Requesters map[string]device.Requester
	// Timeout bounds each plugin call. Default 15s.
	Timeout time.Duration
}

// Service implements hub.SubagentRoutes.
type Service struct {
	o Options
}

var _ hub.SubagentRoutes = (*Service)(nil)

// New returns a Service.
func New(o Options) *Service {
	if o.Timeout == 0 {
		o.Timeout = 15 * time.Second
	}
	return &Service{o: o}
}

// pluginError is an error answer of the plugin or of the dashboard in
// front of it.
type pluginError struct {
	status int
	code   string
}

func (e *pluginError) Error() string {
	return fmt.Sprintf("subagent-routes plugin: %d %s", e.status, e.code)
}

func (s *Service) call(ctx context.Context, user, backendID, method, sessionID string, body any) (hub.Subagents, error) {
	var out hub.Subagents
	if err := s.o.Access.Allowed(ctx, user, backendID); err != nil {
		return out, err
	}
	r := s.o.Requesters[backendID]
	if r == nil {
		return out, hub.ErrNoSubagentRoutes
	}
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return out, err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
	defer cancel()
	resp, err := r.Request(cctx, method, PluginBase+"/sessions/"+url.PathEscape(sessionID), b)
	if err != nil {
		return out, fmt.Errorf("subagent-routes plugin unavailable: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return out, fmt.Errorf("subagent-routes plugin unavailable: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &pluginError{status: resp.StatusCode}
		var raw struct {
			Error any `json:"error"`
		}
		if json.Unmarshal(data, &raw) == nil {
			e.code, _ = raw.Error.(string)
		}
		return out, mapError(e)
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Choice == "" {
		return hub.Subagents{}, errors.New("subagent-routes plugin: bad answer")
	}
	return out, nil
}

func mapError(e *pluginError) error {
	switch {
	case e.code == "bad_choice":
		return fmt.Errorf("%w: that is not a Subagents choice of this chat", hub.ErrBadValue)
	case e.status == http.StatusNotFound || e.status == http.StatusMethodNotAllowed:
		return hub.ErrNoSubagentRoutes
	}
	return e
}

// ChatSubagents reads a chat's Subagents setting.
func (s *Service) ChatSubagents(ctx context.Context, user, backendID, sessionID string) (hub.Subagents, error) {
	return s.call(ctx, user, backendID, http.MethodGet, sessionID, nil)
}

// SetChatSubagents sets a chat's Subagents setting. Its next subagent
// takes it.
func (s *Service) SetChatSubagents(ctx context.Context, user, backendID, sessionID, choice string) (hub.Subagents, error) {
	return s.call(ctx, user, backendID, http.MethodPut, sessionID, map[string]string{"choice": choice})
}
