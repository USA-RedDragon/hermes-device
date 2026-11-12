package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const textNow = "stop, look at this first"

// watch attaches a viewer to the live session, so the test sees what the
// hub passes on.
func watch(t *testing.T, e *env) <-chan hub.Event {
	t.Helper()
	ch, stop, err := e.svc.Attach(context.Background(), userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	next(t, ch)
	return ch
}

func interrupts(e *env) int { return calls(e, gateway.MethodInterrupt) }

func TestStopAndSendRunsAQueuedInputNext(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	watch(t, e)
	ctx := context.Background()
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, textNow)
	if err != nil || res.Steered {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	waitFor(t, "the hub to see the queued input", func() bool {
		_, err := e.svc.StopAndSend(ctx, userAlice, backendPC, sLive, "no-such-input")
		return errors.Is(err, hub.ErrUnknownInput)
	})
	out, err := e.svc.StopAndSend(ctx, userAlice, backendPC, sLive, res.ID)
	if err != nil || out.ID != res.ID {
		t.Fatalf("stop and send = %+v %v", out, err)
	}
	if n := interrupts(e); n != 1 {
		t.Fatalf("interrupts = %d", n)
	}
	waitFor(t, "the queued input to run", func() bool { return slices.Contains(userTexts(t, e), textNow) })
	if n := calls(e, gateway.MethodPromptSubmit); n != 1 {
		t.Fatalf("submits = %d, want only the first", n)
	}
}

func TestStopAndSendSendsAStoppedSteerAgain(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	ch := watch(t, e)
	ctx := context.Background()
	res, err := e.svc.Prompt(hub.WithSteer(ctx), userAlice, backendPC, sLive, textNow)
	if err != nil || !res.Steered {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	adm := e.gw.Steered()[0]
	out, err := e.svc.StopAndSend(ctx, userAlice, backendPC, sLive, res.ID)
	if err != nil || out.ID != hub.SubmissionIDFor("stop-and-send:"+adm) || out.Held {
		t.Fatalf("stop and send = %+v %v", out, err)
	}
	if n := interrupts(e); n != 1 {
		t.Fatalf("interrupts = %d", n)
	}
	waitFor(t, "the steer to run as its own turn", func() bool { return slices.Contains(userTexts(t, e), textNow) })
	var ended map[string]any
	for ended == nil {
		ev := next(t, ch)
		var p map[string]any
		if ev.Kind == hub.KindMessageComplete && json.Unmarshal(ev.Data, &p) == nil && p["outcome"] == "cancelled" {
			ended = p
		}
	}
	if ended[hub.StartSubmissionID] != res.ID {
		t.Fatalf("the stopped steer's end = %v, want submission %s", ended, res.ID)
	}
	again, err := e.svc.StopAndSend(ctx, userAlice, backendPC, sLive, res.ID)
	if !errors.Is(err, hub.ErrUnknownInput) {
		t.Fatalf("second stop and send = %+v %v", again, err)
	}
}

func TestStopAndSendRefusesTheRunningTurn(t *testing.T) {
	t.Parallel()
	e := setup(t, fastSteer())
	running(t, e)
	watch(t, e)
	ctx := context.Background()
	waitFor(t, "the hub to see the turn", func() bool {
		_, err := e.svc.StopAndSend(ctx, userAlice, backendPC, sLive, "tui-1")
		return errors.Is(err, hub.ErrBusy)
	})
	if n := interrupts(e); n != 0 {
		t.Fatalf("interrupts = %d", n)
	}
}
