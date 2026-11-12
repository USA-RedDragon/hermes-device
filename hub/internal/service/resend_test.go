package service_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const pageID = "hub:7f1c2d3e-0000-4000-8000-0123456789ab"

// A page that sends its message again under the same ID, because the
// answer got lost, runs it once.
func TestSameIDRunsOnePrompt(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	ctx := hub.WithSubmissionID(context.Background(), pageID)
	first, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "once")
	if err != nil || first.ID != pageID {
		t.Fatalf("first = %+v %v", first, err)
	}
	again, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "once")
	if err != nil || again.ID != pageID {
		t.Fatalf("again = %+v %v", again, err)
	}
	if n := e.gw.Submits(); n != 1 {
		t.Fatalf("gateway admitted %d inputs, want 1", n)
	}
	if got := userTexts(t, e); len(slices.DeleteFunc(slices.Clone(got), func(s string) bool { return s != "once" })) != 1 {
		t.Fatalf("user messages = %v", got)
	}
}

// A held prompt that the page sends again is held once.
func TestSameIDIsHeldOnce(t *testing.T) {
	t.Parallel()
	e := setup(t, fastHold())
	ctx := hub.WithSubmissionID(context.Background(), pageID)
	ch := attach(t, e.svc)
	e.gw.BeginRestart()
	for range 3 {
		res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "held")
		if err != nil || !res.Held || res.ID != pageID {
			t.Fatalf("prompt = %+v %v", res, err)
		}
	}
	e.gw.FinishRestart()
	until(t, ch, hub.KindInputSent)
	if got := userTexts(t, e); len(slices.DeleteFunc(slices.Clone(got), func(s string) bool { return s != "held" })) != 1 {
		t.Fatalf("user messages = %v", got)
	}
}

// A steer whose request ended early still reaches the running turn, so the
// message does not wait for the turn to end and then run on its own.
func TestSteerOutlivesTheRequest(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	e.gw.SteerScript(gateway.ReasonSteerUnavailable, gateway.ReasonSteerUnavailable)
	ctx, cancel := context.WithCancel(hub.WithSteer(hub.WithSubmissionID(context.Background(), pageID)))
	go func() {
		waitFor(t, "the first steer", func() bool { return calls(e, gateway.MethodSteerQueued) >= 1 })
		cancel()
	}()
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, textSteer)
	if err != nil || !res.Steered {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	if got := e.gw.Steered(); len(got) != 1 {
		t.Fatalf("steered = %v", got)
	}
}

// The page sends a steer again and learns it was handed to the running
// turn, not that it is queued.
func TestSteerSentAgainIsStillSteered(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	ctx := hub.WithSteer(hub.WithSubmissionID(context.Background(), pageID))
	first, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, textSteer)
	if err != nil || !first.Steered {
		t.Fatalf("first = %+v %v", first, err)
	}
	again, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, textSteer)
	if err != nil || !again.Steered || again.ID != pageID {
		t.Fatalf("again = %+v %v", again, err)
	}
	if n := e.gw.Submits(); n != 1 {
		t.Fatalf("gateway admitted %d inputs, want 1", n)
	}
}

// A snapshot names each pending input by the ID the page sent it under.
func TestSnapshotPendingCarriesTheSubmissionID(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	e.gw.AddSession(&gatewaytest.Session{ID: "s-wait", Title: "Waiting", Live: true, StartedAt: 500,
		Pending: []gateway.PendingAdmission{
			{AdmissionID: "a-1", Sequence: 3, Status: gateway.AdmissionQueued, Text: "page", InputID: pageID},
			{AdmissionID: "a-2", Sequence: 4, Status: gateway.AdmissionQueued, Text: "woken", InputID: `["wake","job"]`},
		}})
	ch, stop, err := e.svc.Attach(context.Background(), userAlice, backendPC, "s-wait", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	var snap hub.Snapshot
	if ev := next(t, ch); ev.Kind != hub.KindSnapshot || json.Unmarshal(ev.Data, &snap) != nil {
		t.Fatalf("first event %s", ev.Kind)
	}
	want := []hub.Pending{{ID: "a-1", Status: hub.PendingQueued, Text: "page", SubmissionID: pageID}, {ID: "a-2", Status: hub.PendingQueued, Text: "woken"}}
	if !slices.Equal(snap.Pending, want) {
		t.Fatalf("pending = %+v", snap.Pending)
	}
}
