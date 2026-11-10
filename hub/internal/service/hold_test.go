package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
)

func fastHold() service.Options {
	return service.Options{HoldRetryMin: 20 * time.Millisecond, HoldRetryMax: 100 * time.Millisecond}
}

func held(t *testing.T, e hub.Event) hub.HeldInput {
	t.Helper()
	var in hub.HeldInput
	if err := json.Unmarshal(e.Data, &in); err != nil {
		t.Fatal(err)
	}
	return in
}

// submitted lists the texts of the prompt.submit calls, with the
// submission ID of each.
func submitted(e *env) (texts, ids []string) {
	for _, c := range e.gw.Calls() {
		if c.Method == gateway.MethodPromptSubmit {
			s, _ := c.Params["text"].(string)
			id, _ := c.Params["submission_id"].(string)
			texts, ids = append(texts, s), append(ids, id)
		}
	}
	return texts, ids
}

func userTexts(t *testing.T, e *env) []string {
	t.Helper()
	s, ok := e.gw.Session(sLive)
	if !ok {
		t.Fatal("session gone")
	}
	var out []string
	for _, m := range s.Messages {
		if m["role"] == "user" {
			out = append(out, m["text"].(string)) //nolint:forcetypeassert // the fake writes strings
		}
	}
	return out
}

func attach(t *testing.T, svc *service.Service) <-chan hub.Event {
	t.Helper()
	ch, stop, err := svc.Attach(context.Background(), userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	if e := next(t, ch); e.Kind != hub.KindSnapshot {
		t.Fatalf("first event %s", e.Kind)
	}
	return ch
}

func TestHoldDuringRestartAndSendInOrder(t *testing.T) {
	t.Parallel()
	e := setup(t, fastHold())
	ctx := context.Background()
	ch := attach(t, e.svc)

	e.gw.BeginRestart()
	const textFirst, textSecond = "first", "second"
	first, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, textFirst)
	if err != nil || !first.Held || first.ID == "" || time.Until(first.HeldUntil) < 9*time.Minute {
		t.Fatalf("first = %+v %v", first, err)
	}
	evs := until(t, ch, hub.KindInputHeld)
	if in := held(t, evs[len(evs)-1]); in.ID != first.ID || in.Text != textFirst {
		t.Fatalf("held event = %+v", in)
	}
	e.gw.Stop()
	second, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, textSecond)
	if err != nil || !second.Held {
		t.Fatalf("second = %+v %v", second, err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := e.svc.DeleteSession(ctx, userAlice, backendPC, sLive); !errors.Is(err, hub.ErrBusy) && !gateway.Retryable(err) {
		t.Fatalf("delete with held messages = %v", err)
	}

	e.gw.FinishRestart()
	var sent []string
	for len(sent) < 2 {
		evs := until(t, ch, hub.KindInputSent)
		sent = append(sent, held(t, evs[len(evs)-1]).ID)
	}
	if !slices.Equal(sent, []string{first.ID, second.ID}) {
		t.Fatalf("sent %v, want %v", sent, []string{first.ID, second.ID})
	}
	texts, ids := submitted(e)
	if n := len(ids); n < 2 || !slices.Equal(texts[n-2:], []string{textFirst, textSecond}) ||
		!slices.Equal(ids[n-2:], []string{first.ID, second.ID}) {
		t.Fatalf("submits = %v %v", texts, ids)
	}
	if got := userTexts(t, e); !slices.Equal(got[len(got)-2:], []string{textFirst, textSecond}) {
		t.Fatalf("user messages = %v", got)
	}
	if third, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "third"); err != nil || third.Held {
		t.Fatalf("third = %+v %v", third, err)
	}
}

func TestLostAnswerIsResentOnce(t *testing.T) {
	t.Parallel()
	e := setup(t, fastHold())
	ctx := context.Background()
	ch := attach(t, e.svc)
	e.gw.LoseNextSubmitAnswer()
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "once")
	if err != nil || !res.Held {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	until(t, ch, hub.KindInputSent)
	_, ids := submitted(e)
	if len(ids) != 2 || ids[0] != res.ID || ids[1] != res.ID {
		t.Fatalf("submission ids = %v, want %s twice", ids, res.ID)
	}
	if e.gw.Submits() != 1 {
		t.Fatalf("gateway admitted %d inputs", e.gw.Submits())
	}
	if got := userTexts(t, e); slices.Index(got, "once") != len(got)-1 || len(got) != 2 {
		t.Fatalf("user messages = %v", got)
	}
}

func TestHeldPromptTimesOut(t *testing.T) {
	t.Parallel()
	opts := fastHold()
	opts.HoldTimeout = 300 * time.Millisecond
	e := setup(t, opts)
	ctx := context.Background()
	ch := attach(t, e.svc)
	e.gw.Stop()
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "late")
	if err != nil || !res.Held {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	evs := until(t, ch, hub.KindInputNotSent)
	in := held(t, evs[len(evs)-1])
	if in.ID != res.ID || in.Reason != hub.NotSentTimeout || in.Text != "late" || in.Message == "" {
		t.Fatalf("not sent = %+v", in)
	}
	e.gw.FinishRestart()
	time.Sleep(300 * time.Millisecond)
	if texts, _ := submitted(e); slices.Contains(texts, "late") {
		t.Fatal("timed out prompt was sent")
	}
}

func TestRefusedPromptIsNotHeld(t *testing.T) {
	t.Parallel()
	e := setup(t, fastHold())
	ctx := context.Background()
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, "nope", "x"); !errors.Is(err, hub.ErrUnknownSession) {
		t.Fatalf("unknown session = %v", err)
	}
	if res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "x"); err != nil || res.Held || res.ID == "" {
		t.Fatalf("prompt = %+v %v", res, err)
	}
}

func TestHeldPromptToGoneSessionIsRefused(t *testing.T) {
	t.Parallel()
	e := setup(t, fastHold())
	ctx := context.Background()
	e.gw.Stop()
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, "never-existed", "x")
	if err != nil || !res.Held {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	e.gw.FinishRestart()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := e.svc.Prompt(ctx, userAlice, backendPC, "never-existed", "y"); errors.Is(err, hub.ErrUnknownSession) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("held prompt to a missing session never settled")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDrainGivesUpOnHeldPrompts(t *testing.T) {
	t.Parallel()
	e := setup(t, fastHold())
	ctx := context.Background()
	ch := attach(t, e.svc)
	e.gw.Stop()
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "pending")
	if err != nil || !res.Held {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	e.svc.Drain()
	evs := until(t, ch, hub.KindInputNotSent)
	if in := held(t, evs[len(evs)-1]); in.ID != res.ID || in.Reason != hub.NotSentShutdown {
		t.Fatalf("not sent = %+v", in)
	}
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "after"); !errors.Is(err, hub.ErrShuttingDown) {
		t.Fatalf("prompt after drain = %v", err)
	}
}

func TestHeldPromptShowsAfterNewSnapshot(t *testing.T) {
	t.Parallel()
	e := setup(t, fastHold())
	ctx := context.Background()
	ch := attach(t, e.svc)
	e.gw.Stop()
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "kept")
	if err != nil || !res.Held {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	until(t, ch, hub.KindInputHeld)
	late, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	evs := until(t, late, hub.KindInputHeld)
	if evs[0].Kind != hub.KindSnapshot || held(t, evs[len(evs)-1]).ID != res.ID {
		t.Fatalf("late viewer got %v", kinds(evs))
	}
}

// memRelay is a relay shared by services in one process.
type memRelay struct {
	mu        sync.Mutex
	listeners []func([]byte)
}

func (m *memRelay) Publish(_ context.Context, msg []byte) error {
	m.mu.Lock()
	ls := slices.Clone(m.listeners)
	m.mu.Unlock()
	for _, fn := range ls {
		fn(msg)
	}
	return nil
}

func (m *memRelay) Listen(ctx context.Context, fn func([]byte), _ func(error)) {
	m.mu.Lock()
	m.listeners = append(m.listeners, fn)
	m.mu.Unlock()
	<-ctx.Done()
}

func TestRelayTellsOtherReplicas(t *testing.T) {
	t.Parallel()
	relay := &memRelay{}
	opts := fastHold()
	opts.Relay = relay
	e := setup(t, opts)
	other := service.New(e.cfg.Backends, groups{}, opts)
	t.Cleanup(other.Close)
	ctx := context.Background()
	theirs := attach(t, other)
	ours := attach(t, e.svc)
	deadline := time.Now().Add(5 * time.Second)
	for {
		relay.mu.Lock()
		n := len(relay.listeners)
		relay.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("relay listeners not started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	e.gw.BeginRestart()
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "relayed")
	if err != nil || !res.Held {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	evs := until(t, theirs, hub.KindInputHeld)
	if in := held(t, evs[len(evs)-1]); in.ID != res.ID || in.Text != "relayed" {
		t.Fatalf("other replica got %+v", in)
	}
	late, stopLate, err := other.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stopLate()
	if evs := until(t, late, hub.KindInputHeld); held(t, evs[len(evs)-1]).ID != res.ID {
		t.Fatal("a new viewer on the other replica did not get the held prompt")
	}
	e.gw.FinishRestart()
	evs = until(t, theirs, hub.KindInputSent)
	if in := held(t, evs[len(evs)-1]); in.ID != res.ID {
		t.Fatalf("other replica got %+v", in)
	}
	for _, ev := range until(t, ours, hub.KindInputSent) {
		if ev.Kind == hub.KindInputHeld && held(t, ev).ID != res.ID {
			t.Fatal("own event came back through the relay")
		}
	}
}

func TestStalledSessionDoesNotBlockOthers(t *testing.T) {
	t.Parallel()
	e := setup(t, fastHold())
	ctx := context.Background()
	ch := attach(t, e.svc)
	old, stopOld, err := e.svc.Attach(ctx, userAlice, backendPC, "s-old", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stopOld()
	e.gw.Stop()
	if r, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "parked"); err != nil || !r.Held {
		t.Fatalf("parked = %+v %v", r, err)
	}
	r2, err := e.svc.Prompt(ctx, userAlice, backendPC, "s-old", "other")
	if err != nil || !r2.Held {
		t.Fatalf("other = %+v %v", r2, err)
	}
	e.gw.RefuseSubmits(sLive, gateway.ReasonRuntimeDraining)
	e.gw.FinishRestart()
	evs := until(t, old, hub.KindInputSent)
	if held(t, evs[len(evs)-1]).ID != r2.ID {
		t.Fatal("wrong prompt sent")
	}
	if texts, _ := submitted(e); slices.Contains(userTexts(t, e), "parked") {
		t.Fatalf("parked prompt sent while refused: %v", texts)
	}
	e.gw.RefuseSubmits(sLive, "")
	until(t, ch, hub.KindInputSent)
}

func TestNotSentSaysHermesMayHaveIt(t *testing.T) {
	t.Parallel()
	opts := fastHold()
	opts.HoldTimeout = 400 * time.Millisecond
	e := setup(t, opts)
	ctx := context.Background()
	ch := attach(t, e.svc)
	e.gw.CrashOnNextSubmit()
	res, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "maybe")
	if err != nil || !res.Held {
		t.Fatalf("prompt = %+v %v", res, err)
	}
	evs := until(t, ch, hub.KindInputNotSent)
	if in := held(t, evs[len(evs)-1]); in.Reason != hub.NotSentTimeout || !strings.Contains(in.Message, "may have received") {
		t.Fatalf("not sent = %+v", in)
	}
}

