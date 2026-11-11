package service_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
)

const (
	provAccount1 = "claude-account1"
	modelClaude  = "claude-x"
	kProvider    = "provider"
)

func TestTurnErrorsAndAnnounce(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	got := make(chan service.TurnError, 4)
	e.svc.SetTurnErrors(func(te service.TurnError) { got <- te })
	ctx := context.Background()
	ch, cancel, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	next(t, ch)
	e.gw.Publish(sLive, "session.info", map[string]any{kProvider: provAccount1, "model": modelClaude})
	e.gw.Publish(sLive, "message.complete", map[string]any{"status": "complete", kText: "fine"})
	e.gw.Publish(sLive, "message.complete", map[string]any{"status": "error", kText: "Error",
		"error": "Claude Code is installed but has no usable login (login: /x)"})
	select {
	case te := <-got:
		if te.BackendID != backendPC || te.SessionID != sLive || te.Provider != provAccount1 ||
			!strings.Contains(te.Text, "no usable login") || te.Title != liveTitle {
			t.Fatalf("turn error = %+v", te)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no turn error")
	}
	until(t, ch, hub.KindMessageComplete)

	if !e.svc.Announce(backendPC, sLive, hub.KindClaudeSignIn, hub.ClaudeSignInNotice{Account: "account1", Reason: hub.SignInExpired}) {
		t.Fatal("Announce found no session")
	}
	evs := until(t, ch, hub.KindClaudeSignIn)
	ev := evs[len(evs)-1]
	var n hub.ClaudeSignInNotice
	if err := json.Unmarshal(ev.Data, &n); err != nil || n.Account != "account1" {
		t.Fatalf("announced %s: %v", ev.Data, err)
	}
	if e.svc.Announce(backendPC, "nope", hub.KindClaudeSignIn, n) {
		t.Error("Announce to an unknown session")
	}
	select {
	case te := <-got:
		t.Fatalf("extra turn error %+v", te)
	default:
	}
}

func TestModelsNameClaudeAPI(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	yes := true
	e.gw.Providers = append(e.gw.Providers,
		gateway.ModelProvider{Slug: "anthropic", Name: "Anthropic", Models: []string{modelClaude}, Authenticated: &yes},
		gateway.ModelProvider{Slug: provAccount1, Name: "Claude (account1)", Models: []string{modelClaude}, Authenticated: &yes})
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	m, err := e.svc.Models(ctx, userAlice, backendPC, sLive)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, p := range m.Providers {
		names[p.ID] = p.Name
	}
	if names["anthropic"] != "Claude API" || names[provAccount1] != "Claude (account1)" {
		t.Fatalf("names = %v", names)
	}
}
