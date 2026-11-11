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
	userBob     = "bob"
	roleUser    = "user"
)

type groups map[string][]string

func (g groups) Groups(_ context.Context, user string) ([]string, error) { return g[user], nil }

type env struct {
	gw  *gatewaytest.Gateway
	gw2 *gatewaytest.Gateway
	svc *service.Service
	cfg *config.Config
}

func setup(t *testing.T, opts service.Options) *env {
	t.Helper()
	gw := gatewaytest.New("hub", "pw")
	gw2 := gatewaytest.New("hub", "pw2")
	t.Cleanup(gw.Close)
	t.Cleanup(gw2.Close)
	gw.AddSession(&gatewaytest.Session{ID: sLive, Title: liveTitle, Live: true, StartedAt: 2000,
		Messages: []map[string]any{{"role": roleUser, "text": "hi"}}})
	gw.AddSession(&gatewaytest.Session{ID: "s-old", Title: "Old one", StartedAt: 1000})
	gw2.AddSession(&gatewaytest.Session{ID: "w-1", Title: "Home", Live: true, StartedAt: 3000})
	cfg := &config.Config{LogLevel: "info", Users: []config.User{{Sub: "s1", Name: userAlice}, {Sub: "s2", Name: userBob}}, Backends: []config.Backend{
		{ID: backendPC, Name: "Desktop", URL: gw.URL(), Username: "hub", Password: "pw", Users: []string{userAlice}},
		{ID: backendHome, URL: gw2.URL(), Username: "hub", Password: "pw2", Groups: []string{"home"}},
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	svc := service.New(cfg.Backends, groups{userBob: {"home"}, userAlice: {"admins"}}, opts)
	t.Cleanup(svc.Close)
	return &env{gw: gw, gw2: gw2, svc: svc, cfg: cfg}
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

func TestBackgroundSessionsHidden(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	e.gw.AddSession(&gatewaytest.Session{ID: "bg_abc123", Title: "Background prompt", StartedAt: 3000})
	e.gw.AddSession(&gatewaytest.Session{ID: "bg_124526_be5bed", Title: "CLI background", Live: true, StartedAt: 4000})
	ss, err := e.svc.Sessions(ctx, userAlice)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 || ss[0].ID != sLive || hub.IsBackground(ss[1].ID) {
		t.Fatalf("sessions = %+v", ss)
	}
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, "bg_abc123", 0)
	if err != nil {
		t.Fatalf("open a background prompt's chat: %v", err)
	}
	defer stop()
	select {
	case ev := <-ch:
		if ev.Kind == "" {
			t.Fatalf("first event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot for the background prompt's chat")
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
		if _, err := e.svc.Prompt(ctx, user, backendPC, sLive, "x"); !errors.Is(err, hub.ErrForbidden) {
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
		t.Fatalf("alice on the home backend: %v", err)
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
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "hello"); err != nil {
		t.Fatal(err)
	}
	evs := until(t, ch, hub.KindMessageComplete)
	want := []string{"session.info", hub.KindMessageStart, hub.KindReasoningDelta, hub.KindReasoningDelta, hub.KindToolStart,
		hub.KindToolProgress, hub.KindToolComplete, hub.KindMessageDelta, hub.KindMessageDelta, hub.KindMessageComplete}
	if !slices.Equal(kinds(evs), want) {
		t.Fatalf("kinds = %v, want %v", kinds(evs), want)
	}
	// Hermes sends message.start without text; the hub adds the input's text
	// from the pending list so a replay still shows the user's message.
	if got := text(t, evs[1]); got != "hello" {
		t.Fatalf("message.start text = %q", got)
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
	_, _ = e.svc.Prompt(ctx, userAlice, backendPC, sLive, "one")
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

func TestCronResultIsMarkedAsNote(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ch, stop, err := e.svc.Attach(context.Background(), userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	e.gw.DeliverCron(sLive, "Cronjob Response: ping")
	evs := until(t, ch, hub.KindMessageStart)
	var p struct {
		Text string `json:"text"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal(evs[len(evs)-1].Data, &p); err != nil || p.Text != "Cronjob Response: ping" || p.Note != "cron" {
		t.Fatalf("message.start = %s, %v", evs[len(evs)-1].Data, err)
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
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, sLive, "long job"); err != nil {
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
	_, _ = e.svc.Prompt(ctx, userAlice, backendPC, sLive, "approve me")
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
	if _, err := e.svc.Prompt(ctx, userAlice, backendPC, s.ID, "hi"); err != nil {
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

// turnTwo is the text of the turn that runs.
const turnTwo = "turn two"

func TestAttachSinceNowSkipsEndedTurns(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{ViewerBuffer: 16})
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	e.gw.Publish(sLive, hub.KindMessageDelta, map[string]any{kText: "turn one"})
	e.gw.Publish(sLive, hub.KindMessageComplete, map[string]any{kText: "turn one"})
	e.gw.Publish(sLive, hub.KindMessageDelta, map[string]any{kText: turnTwo})
	for range 3 {
		next(t, ch)
	}
	ch2, stop2, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, hub.SinceNow)
	if err != nil {
		t.Fatal(err)
	}
	defer stop2()
	if k := next(t, ch2).Kind; k != hub.KindSnapshot {
		t.Fatalf("first = %s", k)
	}
	ev := next(t, ch2)
	if ev.Kind != hub.KindMessageDelta || text(t, ev) != turnTwo {
		t.Fatalf("second = %s %s", ev.Kind, ev.Data)
	}
	e.gw.Publish(sLive, hub.KindMessageComplete, map[string]any{kText: turnTwo})
	if k := next(t, ch2).Kind; k != hub.KindMessageComplete {
		t.Fatalf("third = %s", k)
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

const (
	inLost = "a-lost"
	inWait = "a-wait"
)

// A turn lost in a gateway restart holds back the inputs queued after it.
// The hub shows both, dismisses the lost one and cancels a queued one.
func TestLostTurnAndQueuedInputs(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	gen := int64(6)
	e.gw.AddSession(&gatewaytest.Session{ID: "s-stuck", Title: "Stuck", Live: true, StartedAt: 500, Generation: 6,
		Pending: []gateway.PendingAdmission{
			{AdmissionID: inLost, Sequence: 7, Status: gateway.AdmissionUnknown, ExecutionGeneration: &gen, Text: "lost"},
			{AdmissionID: inWait, Sequence: 39, Status: gateway.AdmissionQueued, Text: "waiting"},
		}})
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, "s-stuck", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	pending := func(ev hub.Event) []hub.Pending {
		t.Helper()
		if ev.Kind != hub.KindSnapshot {
			t.Fatalf("event = %s, want snapshot", ev.Kind)
		}
		var s hub.Snapshot
		if err := json.Unmarshal(ev.Data, &s); err != nil {
			t.Fatal(err)
		}
		return s.Pending
	}
	want := []hub.Pending{{ID: inLost, Status: hub.PendingLost, Text: "lost"}, {ID: inWait, Status: hub.PendingQueued, Text: "waiting"}}
	if got := pending(next(t, ch)); !slices.Equal(got, want) {
		t.Fatalf("pending = %+v", got)
	}
	if err := e.svc.CancelInput(ctx, userAlice, backendPC, "s-stuck", inLost); !errors.Is(err, hub.ErrUnknownInput) {
		t.Fatalf("cancel lost: %v", err)
	}
	if err := e.svc.DismissLost(ctx, userAlice, backendPC, "s-stuck", inWait); !errors.Is(err, hub.ErrUnknownInput) {
		t.Fatalf("dismiss queued: %v", err)
	}
	if err := e.svc.DismissLost(ctx, userBob, backendPC, "s-stuck", inLost); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("dismiss by another user: %v", err)
	}
	if err := e.svc.DismissLost(ctx, userAlice, backendPC, "s-stuck", inLost); err != nil {
		t.Fatal(err)
	}
	if got := pending(until(t, ch, hub.KindSnapshot)[0]); !slices.Equal(got, want[1:]) {
		t.Fatalf("after dismiss pending = %+v", got)
	}
	if err := e.svc.CancelInput(ctx, userAlice, backendPC, "s-stuck", inWait); err != nil {
		t.Fatal(err)
	}
	evs := until(t, ch, hub.KindSnapshot)
	if got := pending(evs[len(evs)-1]); len(got) != 0 {
		t.Fatalf("after cancel pending = %+v", got)
	}
	var resolved, cancelled bool
	for _, c := range e.gw.Calls() {
		switch c.Method {
		case gateway.MethodResolveLost:
			resolved = c.Params["admission_id"] == inLost && c.Params["execution_generation"] == float64(6)
		case gateway.MethodPromptCancel:
			cancelled = c.Params["admission_id"] == inWait
		}
	}
	if !resolved || !cancelled {
		t.Fatalf("resolve_unknown sent=%v prompt.cancel sent=%v", resolved, cancelled)
	}
}

// TestJoinSessionsFromEditorsAndCLI checks that sessions VS Code (source
// acp) and the CLI started are listed with their source and that the hub
// joins them live: a viewer sees what the other surface does, and a
// prompt from the hub goes to the same session.
func TestJoinSessionsFromEditorsAndCLI(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	e.gw.AddSession(&gatewaytest.Session{ID: "s-code", Title: "From VS Code", Live: true, StartedAt: 4000, Source: gateway.SourceACP,
		Messages: []map[string]any{{"role": roleUser, "text": "fix the build"}}})
	e.gw.AddSession(&gatewaytest.Session{ID: "s-cli", Title: "From the CLI", Live: true, StartedAt: 5000, Source: gateway.SourceCLI})
	ss, err := e.svc.Sessions(ctx, userAlice)
	if err != nil {
		t.Fatal(err)
	}
	src := map[string]string{}
	for _, s := range ss {
		src[s.ID] = s.Source
	}
	if src["s-code"] != gateway.SourceACP || src["s-cli"] != gateway.SourceCLI {
		t.Fatalf("sources = %v", src)
	}
	for _, id := range []string{"s-code", "s-cli"} {
		ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if k := next(t, ch).Kind; k != hub.KindSnapshot {
			t.Fatalf("%s: first event = %s", id, k)
		}
		e.gw.Publish(id, "message.delta", map[string]any{kText: "from the editor"})
		if ev := until(t, ch, hub.KindMessageDelta); text(t, ev[len(ev)-1]) != "from the editor" {
			t.Fatalf("%s: live event = %s", id, ev[len(ev)-1].Data)
		}
		if _, err := e.svc.Prompt(ctx, userAlice, backendPC, id, "and from the hub"); err != nil {
			t.Fatal(err)
		}
		until(t, ch, hub.KindMessageComplete)
		stop()
	}
}

func TestNewSessionAtSendsRowCWD(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	s, err := e.svc.NewSessionAt(ctx, userAlice, backendPC, "/opt/placeholder", "/home/alice/src")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := e.gw.Session(s.ID)
	if !ok || got.CWD != "/opt/placeholder" || got.RowCWD != "/home/alice/src" || got.Source != gateway.SourceGUI || s.Source != gateway.SourceGUI {
		t.Fatalf("session = %+v, %+v", got, s)
	}
	if _, err := e.svc.NewSessionAt(ctx, userBob, backendPC, "/p", "/home/bob"); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("bob on alice's backend = %v", err)
	}
	plain, err := e.svc.NewSession(ctx, userAlice, backendPC)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := e.gw.Session(plain.ID); got.RowCWD != "" {
		t.Fatalf("plain session row_cwd = %q", got.RowCWD)
	}
}
