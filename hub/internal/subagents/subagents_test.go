package subagents_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/device"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/subagents"
)

const (
	alice       = "alice"
	backendMain = "main"
	bare        = "bare"
	haiku       = "haiku"
	haikuName   = "Haiku 5.5"
)

type access struct{}

func (access) Allowed(_ context.Context, user, id string) error {
	if user == alice && (id == backendMain || id == bare) {
		return nil
	}
	return hub.ErrForbidden
}

// plugin is a fake subagent-routes plugin that keeps one choice per session.
type plugin struct {
	mu      sync.Mutex
	choices map[string]string
	paths   []string
}

func (p *plugin) Request(_ context.Context, method, path string, body []byte) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paths = append(p.paths, method+" "+path)
	rec := httptest.NewRecorder()
	session, ok := strings.CutPrefix(path, subagents.PluginBase+"/sessions/")
	if !ok {
		rec.WriteHeader(http.StatusNotFound)
		return rec.Result(), nil
	}
	if method == http.MethodPut {
		var b struct {
			Choice string `json:"choice"`
		}
		_ = json.Unmarshal(body, &b)
		if b.Choice != hub.SubagentsAuto && b.Choice != haiku {
			rec.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(rec, `{"error":"bad_choice"}`)
			return rec.Result(), nil
		}
		p.choices[session] = b.Choice
	}
	choice := p.choices[session]
	if choice == "" {
		choice = hub.SubagentsAuto
	}
	err := json.NewEncoder(rec).Encode(hub.Subagents{Choice: choice, Choices: []hub.SubagentChoice{
		{ID: hub.SubagentsAuto, Label: "Auto (model chooses)", Line: "The model picks."},
		{ID: haiku, Label: haikuName, Group: "Every subagent on", Line: "Every subagent runs on " + haikuName + ".", Em: haikuName},
	}})
	return rec.Result(), err
}

// missing is a Hermes without the plugin.
type missing struct{}

func (missing) Request(context.Context, string, string, []byte) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusNotFound)
	return rec.Result(), nil
}

func newService() (*subagents.Service, *plugin) {
	p := &plugin{choices: map[string]string{}}
	return subagents.New(subagents.Options{Access: access{},
		Requesters: map[string]device.Requester{backendMain: p, bare: missing{}}}), p
}

func TestChatSubagents(t *testing.T) {
	t.Parallel()
	svc, p := newService()
	ctx := context.Background()
	got, err := svc.ChatSubagents(ctx, alice, backendMain, "s/1")
	if err != nil || got.Choice != hub.SubagentsAuto || len(got.Choices) != 2 {
		t.Fatalf("ChatSubagents = %+v, %v", got, err)
	}
	if p.paths[0] != "GET "+subagents.PluginBase+"/sessions/s%2F1" {
		t.Fatalf("path = %q", p.paths[0])
	}
	got, err = svc.SetChatSubagents(ctx, alice, backendMain, "s/1", haiku)
	if err != nil || got.Choice != haiku {
		t.Fatalf("SetChatSubagents = %+v, %v", got, err)
	}
	if cur, ok := got.Current(); !ok || cur.Label != haikuName {
		t.Fatalf("Current = %+v, %v", cur, ok)
	}
	if _, err := svc.SetChatSubagents(ctx, alice, backendMain, "s/1", "fable"); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("bad choice: %v", err)
	}
}

func TestChatSubagentsRefusals(t *testing.T) {
	t.Parallel()
	svc, _ := newService()
	ctx := context.Background()
	if _, err := svc.ChatSubagents(ctx, "mallory", backendMain, "s1"); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("other user: %v", err)
	}
	if _, err := svc.ChatSubagents(ctx, alice, bare, "s1"); !errors.Is(err, hub.ErrNoSubagentRoutes) {
		t.Fatalf("no plugin: %v", err)
	}
	if _, err := svc.ChatSubagents(ctx, alice, "unknown", "s1"); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("unknown backend: %v", err)
	}
}
