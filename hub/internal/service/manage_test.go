package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

const (
	provLocal  = "local"
	modelBig   = "big"
	modelSmall = "small"
)

func sessionByID(t *testing.T, e *env, id string) (hub.Session, bool) {
	t.Helper()
	ss, err := e.svc.Sessions(context.Background(), userAlice)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.ID == id {
			return s, true
		}
	}
	return hub.Session{}, false
}

func TestSnapshotCarriesSessionInfo(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ch, stop, err := e.svc.Attach(context.Background(), userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	var snap hub.Snapshot
	if err := json.Unmarshal(next(t, ch).Data, &snap); err != nil {
		t.Fatal(err)
	}
	i := snap.Info
	if i.Model != modelSmall || i.Provider != provLocal || i.ContextLength != 131072 || !strings.Contains(string(i.Usage), `"calls":0`) {
		t.Fatalf("info = %+v", i)
	}
}

// The hub fences session.mutate on the revision it last saw. Turns and
// other clients move the revision; a stale fence is retried once with the
// fresh one the refusal carries.
func TestRenameArchiveAndDelete(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "hi"); err != nil {
		t.Fatal(err)
	}
	until(t, ch, hub.KindMessageComplete)
	e.gw.BumpRevision(sLive)
	if err := e.svc.RenameSession(ctx, userAlice, backendPC, sLive, "  Trip plans  "); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.gw.Session(sLive); s.Title != "Trip plans" {
		t.Fatalf("title = %q", s.Title)
	}
	ev := until(t, ch, gateway.UpdatedEvent)
	if !strings.Contains(string(ev[len(ev)-1].Data), `"title":"Trip plans"`) {
		t.Fatalf("session.updated = %s", ev[len(ev)-1].Data)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", 101), "two\nlines"} {
		if err := e.svc.RenameSession(ctx, userAlice, backendPC, sLive, bad); !errors.Is(err, hub.ErrBadValue) {
			t.Fatalf("rename %q: %v", bad, err)
		}
	}
	if err := e.svc.RenameSession(ctx, userAlice, backendPC, sLive, "Old one"); !errors.Is(err, hub.ErrTitleInUse) {
		t.Fatalf("rename to a taken title: %v", err)
	}
	if err := e.svc.RenameSession(ctx, userBob, backendPC, sLive, "x"); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("rename by another user: %v", err)
	}

	if err := e.svc.ArchiveSession(ctx, userAlice, backendPC, sLive, true); err != nil {
		t.Fatal(err)
	}
	if s, ok := sessionByID(t, e, sLive); !ok || !s.Archived || s.Title != "Trip plans" {
		t.Fatalf("archived session = %+v %v", s, ok)
	}
	if err := e.svc.ArchiveSession(ctx, userAlice, backendPC, sLive, false); err != nil {
		t.Fatal(err)
	}
	if s, ok := sessionByID(t, e, sLive); !ok || s.Archived {
		t.Fatalf("unarchived session = %+v %v", s, ok)
	}

	e.gw.HoldTurns = true
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "hold"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteSession(ctx, userAlice, backendPC, sLive); !errors.Is(err, hub.ErrBusy) {
		t.Fatalf("delete while running: %v", err)
	}
	if err := e.svc.Interrupt(ctx, userAlice, backendPC, sLive); err != nil {
		t.Fatal(err)
	}
	until(t, ch, hub.KindMessageComplete)
	if err := e.svc.DeleteSession(ctx, userAlice, backendPC, sLive); err != nil {
		t.Fatal(err)
	}
	until(t, ch, hub.KindSessionDeleted)
	if ev, ok := <-ch; ok {
		t.Fatalf("stream still open after delete: %s", ev.Kind)
	}
	if _, ok := e.gw.Session(sLive); ok {
		t.Fatal("gateway still has the session")
	}
	if _, ok := sessionByID(t, e, sLive); ok {
		t.Fatal("deleted session still listed")
	}
	if _, _, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0); !errors.Is(err, hub.ErrUnknownSession) {
		t.Fatalf("attach to a deleted session: %v", err)
	}
	var fenced bool
	for _, c := range e.gw.Calls() {
		if c.Method == gateway.MethodMutate && c.Params["operation"] == gateway.OpDelete {
			_, fenced = c.Params["expected_generation"]
		}
	}
	if !fenced {
		t.Fatal("delete sent without expected_generation")
	}
}

func TestModelsAndReasoning(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
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
	if m.Model != modelSmall || m.Provider != provLocal || len(m.Providers) != 1 || m.Providers[0].ID != provLocal {
		t.Fatalf("models = %+v", m)
	}
	want := []hub.ModelChoice{{ID: modelSmall, Label: modelSmall, CanDisableReasoning: true},
		{ID: modelBig, Label: modelBig, Reasoning: true, CanDisableReasoning: true}}
	if got := m.Providers[0].Models; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("local models = %+v", got)
	}

	res, err := e.svc.SetModel(ctx, userAlice, backendPC, sLive, provLocal, modelBig, "")
	if err != nil || res.Status != hub.ModelChanged {
		t.Fatalf("set model = %+v, %v", res, err)
	}
	if s, _ := e.gw.Session(sLive); s.Model != modelBig {
		t.Fatalf("model = %q", s.Model)
	}
	evs := until(t, ch, gateway.InfoEvent)
	if !strings.Contains(string(evs[len(evs)-1].Data), `"model":"big"`) {
		t.Fatalf("session.info = %s", evs[len(evs)-1].Data)
	}
	if _, err := e.svc.SetModel(ctx, userAlice, backendPC, sLive, provLocal, "nope", ""); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("set unknown model: %v", err)
	}

	e.gw.RequireConfirm(modelSmall)
	res, err = e.svc.SetModel(ctx, userAlice, backendPC, sLive, provLocal, modelSmall, "")
	if err != nil || res.Status != hub.ModelConfirm || res.Confirm == "" || res.Message == "" {
		t.Fatalf("set model needing confirmation = %+v, %v", res, err)
	}
	if s, _ := e.gw.Session(sLive); s.Model != modelBig {
		t.Fatalf("model changed before confirmation: %q", s.Model)
	}
	res, err = e.svc.SetModel(ctx, userAlice, backendPC, sLive, provLocal, modelSmall, res.Confirm)
	if err != nil || res.Status != hub.ModelChanged {
		t.Fatalf("confirmed set model = %+v, %v", res, err)
	}

	for _, c := range []struct{ in, want string }{{"High", "high"}, {"none", "none"}, {"", ""}} {
		if err := e.svc.SetReasoning(ctx, userAlice, backendPC, sLive, c.in); err != nil {
			t.Fatalf("reasoning %q: %v", c.in, err)
		}
		if s, _ := e.gw.Session(sLive); s.Effort != c.want {
			t.Fatalf("effort after %q = %q", c.in, s.Effort)
		}
	}
	if err := e.svc.SetReasoning(ctx, userAlice, backendPC, sLive, "turbo"); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("bad effort: %v", err)
	}

	e.gw.HoldTurns = true
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "hold"); err != nil {
		t.Fatal(err)
	}
	res, err = e.svc.SetModel(ctx, userAlice, backendPC, sLive, provLocal, modelBig, "")
	if err != nil || res.Status != hub.ModelDeferred {
		t.Fatalf("set model while running = %+v, %v", res, err)
	}
	if _, err := e.svc.Models(ctx, userBob, backendPC, sLive); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("models for another user: %v", err)
	}
}
