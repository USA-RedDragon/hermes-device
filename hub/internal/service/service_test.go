package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
)

const (
	backendPC   = "desktop"
	backendHome = "cluster-home"
	liveTitle   = "Live one"
	kText       = "text"
	sLive       = "s-live"
	userAlice   = "alice"
	userBob   = "bob"
)

type groups map[string][]string

func (g groups) Groups(_ context.Context, user string) ([]string, error) { return g[user], nil }

type env struct {
	gw  *gatewaytest.Gateway
	gw2 *gatewaytest.Gateway
	svc *service.Service
}

func setup(t *testing.T, opts service.Options) *env {
	t.Helper()
	gw := gatewaytest.New("hub", "pw")
	gw2 := gatewaytest.New("hub", "pw2")
	t.Cleanup(gw.Close)
	t.Cleanup(gw2.Close)
	gw.AddSession(&gatewaytest.Session{ID: sLive, Title: liveTitle, Live: true, StartedAt: 2000,
		Messages: []map[string]any{{"role": "user", "text": "hi"}}})
	gw.AddSession(&gatewaytest.Session{ID: "s-old", Title: "Old one", StartedAt: 1000})
	gw2.AddSession(&gatewaytest.Session{ID: "w-1", Title: "Home's", Live: true, StartedAt: 3000})
	cfg := &config.Config{LogLevel: "info", Users: []config.User{{Sub: "s1", Name: userAlice}, {Sub: "s2", Name: userBob}}, Backends: []config.Backend{
		{ID: backendPC, Name: "Desktop", URL: gw.URL(), Username: "hub", Password: "pw", Users: []string{userAlice}},
		{ID: backendHome, URL: gw2.URL(), Username: "hub", Password: "pw2", Groups: []string{"home"}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	svc := service.New(cfg.Backends, groups{userBob: {"home"}, userAlice: {"admins"}}, opts)
	t.Cleanup(svc.Close)
	return &env{gw: gw, gw2: gw2, svc: svc}
}

func next(t *testing.T, ch <-chan hub.Event) hub.Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return hub.Event{}
}

// until reads events until one of kind arrives and returns all of them.
func until(t *testing.T, ch <-chan hub.Event, kind string) []hub.Event {
	t.Helper()
	var out []hub.Event
	for {
		e := next(t, ch)
		out = append(out, e)
		if e.Kind == kind {
			return out
		}
	}
}

func kinds(evs []hub.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func text(t *testing.T, e hub.Event) string {
	t.Helper()
	var p struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(e.Data, &p)
	return p.Text
}

func TestBackendsAndSessions(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	bs, err := e.svc.Backends(ctx, userAlice)
	if err != nil || len(bs) != 1 || bs[0].ID != backendPC || bs[0].Name != "Desktop" {
		t.Fatalf("alice backends = %v, %v", bs, err)
	}
	bs, _ = e.svc.Backends(ctx, userBob)
	if len(bs) != 1 || bs[0].ID != backendHome || bs[0].Name != backendHome {
		t.Fatalf("bob backends = %v", bs)
	}
	ss, err := e.svc.Sessions(ctx, userAlice)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 || ss[0].ID != sLive || !ss[0].Live || ss[1].ID != "s-old" || ss[1].Live {
		t.Fatalf("sessions = %+v", ss)
	}
	if ss[0].BackendID != backendPC || ss[0].Title != liveTitle || ss[0].UpdatedAt.Unix() != 2000 {
		t.Fatalf("session fields = %+v", ss[0])
	}
}

func TestAuthorizationDenied(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	for _, user := range []string{userBob, "mallory", ""} {
		if _, _, err := e.svc.Attach(ctx, user, backendPC, sLive, 0); !errors.Is(err, hub.ErrForbidden) {
			t.Fatalf("%q attach: %v", user, err)
		}
		if err := e.svc.Prompt(ctx, user, backendPC, sLive, "x"); !errors.Is(err, hub.ErrForbidden) {
			t.Fatalf("%q prompt: %v", user, err)
		}
		if err := e.svc.Interrupt(ctx, user, backendPC, sLive); !errors.Is(err, hub.ErrForbidden) {
			t.Fatalf("%q interrupt: %v", user, err)
		}
		if err := e.svc.RespondApproval(ctx, user, backendPC, sLive, "ap-1", true); !errors.Is(err, hub.ErrForbidden) {
			t.Fatalf("%q approval: %v", user, err)
		}
		if _, err := e.svc.NewSession(ctx, user, backendPC); !errors.Is(err, hub.ErrForbidden) {
			t.Fatalf("%q new session: %v", user, err)
		}
	}
	ss, _ := e.svc.Sessions(ctx, userBob)
	if len(ss) != 1 || ss[0].ID != "w-1" {
		t.Fatalf("bob sessions = %+v", ss)
	}
	if _, _, err := e.svc.Attach(ctx, userAlice, backendHome, "w-1", 0); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("alice on home's backend: %v", err)
	}
	if _, _, err := e.svc.Attach(ctx, userAlice, "nope", "w-1", 0); !errors.Is(err, hub.ErrUnknownBackend) {
		t.Fatalf("unknown backend: %v", err)
	}
	for _, c := range e.gw.Calls() {
		if c.Method != gateway.MethodSessionList {
			t.Fatalf("denied user reached the gateway: %v", c)
		}
	}
}

func TestAttachStreamsInOrder(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	snap := next(t, ch)
	if snap.Kind != hub.KindSnapshot {
		t.Fatalf("first event = %s", snap.Kind)
	}
	var s hub.Snapshot
	if err := json.Unmarshal(snap.Data, &s); err != nil || s.SessionID != sLive || s.Title != liveTitle || len(s.Messages) != 1 {
		t.Fatalf("snapshot = %+v, %v", s, err)
	}
	if !e.gw.ShowReasoning(sLive) {
		t.Fatal("show_reasoning was not turned on")
	}
	if err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "hello"); err != nil {
		t.Fatal(err)
	}
	evs := until(t, ch, hub.KindMessageComplete)
	want := []string{"message.start", hub.KindReasoningDelta, hub.KindReasoningDelta, hub.KindToolStart,
		hub.KindToolProgress, hub.KindToolComplete, hub.KindMessageDelta, hub.KindMessageDelta, hub.KindMessageComplete}
	if !slices.Equal(kinds(evs), want) {
		t.Fatalf("kinds = %v, want %v", kinds(evs), want)
	}
	last := snap.Seq
	for _, ev := range evs {
		if ev.Seq <= last {
			t.Fatalf("seq not increasing: %d after %d", ev.Seq, last)
		}
		last = ev.Seq
	}
	if text(t, evs[len(evs)-1]) != "echo: hello" {
		t.Fatalf("final text = %q", text(t, evs[len(evs)-1]))
	}
	ch2, stop2, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	evs2 := until(t, ch2, hub.KindMessageComplete)
	if evs2[0].Kind != hub.KindSnapshot || !slices.Equal(kinds(evs2[1:]), want) {
		t.Fatalf("second viewer = %v", kinds(evs2))
	}
	resumes := 0
	for _, c := range e.gw.Calls() {
		if c.Method == gateway.MethodSessionResume {
			resumes++
		}
	}
	if resumes != 1 {
		t.Fatalf("resumes = %d, want 1 shared subscription", resumes)
	}
}

func TestViewerReconnectSinceSeq(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	next(t, ch)
	_ = e.svc.Prompt(ctx, userAlice, backendPC, sLive, "one")
	evs := until(t, ch, hub.KindMessageComplete)
	stop()
	cut := evs[3].Seq
	ch2, stop2, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, cut)
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	got := until(t, ch2, hub.KindMessageComplete)
	if !slices.Equal(kinds(got), kinds(evs[4:])) || got[0].Seq != evs[4].Seq {
		t.Fatalf("replay = %v, want %v", kinds(got), kinds(evs[4:]))
	}
	ch3, stop3, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer stop3()
	if k := next(t, ch3).Kind; k != hub.KindSnapshot {
		t.Fatalf("stale cursor got %s, want snapshot", k)
	}
}

func TestGatewayReconnectReplaysWithEventsSince(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	e.gw.Publish(sLive, "message.delta", map[string]any{kText: "a"})
	if text(t, next(t, ch)) != "a" {
		t.Fatal("live delta missing")
	}
	e.gw.PublishOffline(sLive, "message.delta", map[string]any{kText: "b"})
	e.gw.PublishOffline(sLive, "message.delta", map[string]any{kText: "c"})
	e.gw.DropConnections()
	evs := until(t, ch, hub.KindBackendStatus)
	if len(evs) != 1 || string(evs[0].Data) != `{"connected":false}` {
		t.Fatalf("got %v %s", kinds(evs), evs[0].Data)
	}
	evs = until(t, ch, hub.KindBackendStatus)
	if !slices.Equal(kinds(evs), []string{hub.KindMessageDelta, hub.KindMessageDelta, hub.KindBackendStatus}) ||
		text(t, evs[0]) != "b" || text(t, evs[1]) != "c" || string(evs[2].Data) != `{"connected":true}` {
		t.Fatalf("after reconnect = %v", kinds(evs))
	}
	var since *gatewaytest.Call
	for _, c := range e.gw.Calls() {
		if c.Method == gateway.MethodEventsSince {
			since = &c
		}
	}
	if since == nil || since.Params["last_sequence"] != float64(1) || since.Params["replay_epoch"] == "" {
		t.Fatalf("events.since call = %+v", since)
	}
	e.gw.Publish(sLive, "message.delta", map[string]any{kText: "d"})
	if ev := next(t, ch); text(t, ev) != "d" {
		t.Fatalf("live after reconnect = %s %s", ev.Kind, ev.Data)
	}
}

func TestAttachMidTurnBackfillsCurrentTurn(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	e.gw.Publish(sLive, "message.complete", map[string]any{kText: "earlier turn"})
	e.gw.AddSession(&gatewaytest.Session{ID: "s-run", Live: true, Running: true})
	e.gw.Publish("s-run", "message.complete", map[string]any{kText: "old"})
	e.gw.Publish("s-run", "message.start", map[string]any{})
	e.gw.Publish("s-run", "tool.start", map[string]any{"tool_id": "t9", "name": "terminal"})
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, "s-run", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	got := []string{next(t, ch).Kind, next(t, ch).Kind, next(t, ch).Kind}
	if !slices.Equal(got, []string{hub.KindSnapshot, "message.start", hub.KindToolStart}) {
		t.Fatalf("got %v", got)
	}
}

func TestInterrupt(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	e.gw.HoldTurns = true
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	if err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "long job"); err != nil {
		t.Fatal(err)
	}
	e.gw.Publish(sLive, "message.start", map[string]any{})
	until(t, ch, "message.start")
	if err := e.svc.Interrupt(ctx, userAlice, backendPC, sLive); err != nil {
		t.Fatal(err)
	}
	ev := until(t, ch, hub.KindMessageComplete)
	if !json.Valid(ev[len(ev)-1].Data) || string(ev[len(ev)-1].Data) != `{"status":"interrupted","text":""}` {
		t.Fatalf("interrupt event = %s", ev[len(ev)-1].Data)
	}
}

func TestApproval(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	if err := e.svc.RespondApproval(ctx, userAlice, backendPC, sLive, "ap-1", true); !errors.Is(err, hub.ErrUnknownApproval) {
		t.Fatalf("unknown approval: %v", err)
	}
	_ = e.svc.Prompt(ctx, userAlice, backendPC, sLive, "approve me")
	evs := until(t, ch, hub.KindApprovalRequest)
	var p gateway.Prompt
	_ = json.Unmarshal(evs[len(evs)-1].Data, &p)
	if p.PromptID != "ap-1" || *p.Command != "rm -rf build" {
		t.Fatalf("approval = %+v", p)
	}
	if err := e.svc.RespondApproval(ctx, userAlice, backendPC, sLive, "ap-1", false); err != nil {
		t.Fatal(err)
	}
	evs = until(t, ch, hub.KindMessageComplete)
	if text(t, evs[len(evs)-1]) != "approved: deny" {
		t.Fatalf("result = %s", evs[len(evs)-1].Data)
	}
	if err := e.svc.RespondApproval(ctx, userAlice, backendPC, sLive, "ap-1", true); !errors.Is(err, hub.ErrUnknownApproval) {
		t.Fatalf("settled approval: %v", err)
	}
}

func TestNewSessionAndIdleDetach(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{IdleDetach: 50 * time.Millisecond})
	ctx := context.Background()
	s, err := e.svc.NewSession(ctx, userAlice, backendPC)
	if err != nil || s.ID == "" || !s.Live {
		t.Fatalf("new session = %+v, %v", s, err)
	}
	if err := e.svc.Prompt(ctx, userAlice, backendPC, s.ID, "hi"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		detached := false
		for _, c := range e.gw.Calls() {
			detached = detached || c.Method == gateway.MethodSessionDetach
		}
		if detached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle session never detached")
		}
		time.Sleep(20 * time.Millisecond)
	}
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, s.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if k := next(t, ch).Kind; k != hub.KindSnapshot {
		t.Fatalf("re-attach got %s", k)
	}
}

func TestLoginAgainWhenCookieExpires(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	if _, err := e.svc.Sessions(ctx, userAlice); err != nil {
		t.Fatal(err)
	}
	e.gw.ExpireLogins()
	e.gw.DropConnections()
	time.Sleep(50 * time.Millisecond)
	ss, err := e.svc.Sessions(ctx, userAlice)
	if err != nil || len(ss) != 2 {
		t.Fatalf("sessions after expiry = %v, %v", ss, err)
	}
	if n := e.gw.Logins(); n != 2 {
		t.Fatalf("logins = %d, want 2", n)
	}
	st, _ := e.svc.BackendStatus(ctx, userAlice)
	if st[backendPC] != "" {
		t.Fatalf("status = %v", st)
	}
}

func TestLateViewerGetsWholeBuffer(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{ViewerBuffer: 16})
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	for range 40 {
		e.gw.Publish(sLive, hub.KindMessageDelta, map[string]any{kText: "x"})
		next(t, ch)
	}
	ch2, stop2, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	if k := next(t, ch2).Kind; k != hub.KindSnapshot {
		t.Fatalf("first = %s", k)
	}
	for i := range 40 {
		if k := next(t, ch2).Kind; k != hub.KindMessageDelta {
			t.Fatalf("event %d = %s", i, k)
		}
	}
}

func TestReplayGapTakesNewSnapshot(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	e.gw.RetireSubscriptions(sLive)
	if k := next(t, ch).Kind; k != hub.KindSnapshot {
		t.Fatalf("after gap = %s", k)
	}
	e.gw.Publish(sLive, hub.KindMessageDelta, map[string]any{kText: "after"})
	if ev := next(t, ch); text(t, ev) != "after" {
		t.Fatalf("live after gap = %s %s", ev.Kind, ev.Data)
	}
}
