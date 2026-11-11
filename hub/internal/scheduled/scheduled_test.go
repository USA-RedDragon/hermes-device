package scheduled_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/scheduled"
	"github.com/USA-RedDragon/hermes-hub/internal/scheduled/scheduledtest"
)

const (
	alice   = "alice"
	bob     = "bob"
	carol   = "carol"
	backend = "pc"
)

// Fields of the fake's job records and run rows.
const (
	kName    = "name"
	kNext    = "next_run_at"
	kEnabled = "enabled"
	kOwner   = "talaria_owner"
	kDeliver = "deliver"
	kEnded   = "ended_at"
	kindCron = "cron"
	devAlice = "d_alice"
	devBob   = "d_bob"
	custom   = "custom"
	sandbox  = "Sandbox"
	claimed  = "Claimed"
	small    = "qwen-small"
	paused   = "paused"
	kScript  = "script"
	kSource  = "source"
	kState   = "state"
	kStarted = "started_at"
)

type access struct{ users []string }

func (a access) Allowed(_ context.Context, user, backendID string) error {
	if backendID != backend {
		return hub.ErrUnknownBackend
	}
	if !slices.Contains(a.users, user) {
		return hub.ErrForbidden
	}
	return nil
}

type devices map[string]hub.Device

func (d devices) Device(_ context.Context, id string) (hub.Device, bool, error) {
	dev, ok := d[id]
	return dev, ok, nil
}

type env struct {
	svc  *scheduled.Service
	dash *scheduledtest.Dash
	gw   *gatewaytest.Gateway
	loc  *time.Location
	now  time.Time
}

// setup serves a backend that users may use, with carol never allowed.
func setup(t *testing.T, users ...string) *env {
	t.Helper()
	gw := gatewaytest.New("hub", "pw")
	t.Cleanup(gw.Close)
	dash := scheduledtest.New(gw)
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 11, 20, 0, 0, 0, loc)
	dash.Now = func() time.Time { return now }
	d := gateway.NewDialer(gateway.Credentials{BaseURL: gw.URL(), Provider: "basic", Username: "hub", Password: "pw"})
	b := config.Backend{ID: backend, Name: "PC", Users: users, Devices: true,
		ServiceDevices: []config.ServiceDevice{{ID: "sandbox-alice", Name: sandbox}}}
	svc := scheduled.New(scheduled.Options{Access: access{users}, Backends: []config.Backend{b},
		Requesters: map[string]scheduled.Requester{backend: d},
		Devices: devices{
			devAlice: {ID: devAlice, User: alice, Name: "upstairs-pc"},
			devBob:   {ID: devBob, User: bob, Name: "bobs-laptop"},
		},
		Location: loc, Now: func() time.Time { return now }})
	return &env{svc: svc, dash: dash, gw: gw, loc: loc, now: now}
}

func (e *env) choices(t *testing.T, jobs map[string]string, owners map[string]string) {
	t.Helper()
	doc := map[string]any{"version": 1, "sessions": map[string]any{}, "device_jobs": map[string]any{}}
	js, os := map[string]any{}, map[string]any{}
	for id, dev := range jobs {
		js[id] = map[string]string{"device_id": dev, "user": alice}
	}
	for id, u := range owners {
		os[id] = map[string]string{"user": u}
	}
	doc["jobs"], doc["job_owners"] = js, os
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	e.gw.SetFile(scheduledtest.Home+"/device/browser.json", raw)
}

func find(t *testing.T, jobs hub.Jobs, name string) hub.Job {
	t.Helper()
	for _, j := range jobs.Jobs {
		if j.Name == name {
			return j
		}
	}
	t.Fatalf("no job %q in %+v", name, jobs.Jobs)
	return hub.Job{}
}

func names(jobs hub.Jobs) []string {
	out := make([]string, 0, len(jobs.Jobs))
	for _, j := range jobs.Jobs {
		out = append(out, j.Name)
	}
	slices.Sort(out)
	return out
}

func TestJobsShowStateTimesAndRuns(t *testing.T) {
	t.Parallel()
	e := setup(t, alice)
	ctx := context.Background()
	e.dash.Add(map[string]any{"id": "a1", kName: "Weekly review", "schedule_display": "every tuesday 1am",
		"schedule": map[string]any{"kind": kindCron, "expr": "0 1 * * 2", "display": "every tuesday 1am"},
		kNext:      "2026-11-17T01:00:00-06:00", "skills": []string{"sev1-tracker"}})
	e.dash.Add(map[string]any{"id": "a2", kName: "Sign-in check", "last_status": "error",
		"last_error": "Needs login: 401 Unauthorized at the SSO page", "model": small, "provider": custom})
	e.dash.Add(map[string]any{"id": "a3", kName: "Digest", "last_status": "error", "last_error": "gh: API rate limit exceeded"})
	e.dash.Add(map[string]any{"id": "a4", kName: "Backlog", kEnabled: false, kState: paused, kNext: nil})
	e.dash.Add(map[string]any{"id": "a5", kName: "Talaria draft", "paused_reason": "Talaria draft: not saved yet",
		kOwner: alice, kEnabled: false, kState: paused})
	e.dash.Add(map[string]any{"id": "a6", kName: "From a chat", kDeliver: "origin", kScript: "x.py"})
	started := float64(e.now.Add(-26 * time.Hour).Unix())
	e.dash.Runs["a1"] = []map[string]any{
		{"id": "s_new", kSource: kindCron, kStarted: float64(e.now.Add(-time.Minute).Unix()), kEnded: nil,
			"is_active": true, "scheduler_owned": true, "title": "secret chat title"},
		{"id": "s_old", kSource: kindCron, kStarted: started, kEnded: started + 161, "title": "secret chat title"},
	}
	e.dash.Runs["a3"] = []map[string]any{{"id": "s_fail", kSource: kindCron, kStarted: started, kEnded: started + 12}}
	e.choices(t, map[string]string{"a1": devAlice}, nil)

	jobs, err := e.svc.Jobs(ctx, alice)
	if err != nil {
		t.Fatal(err)
	}
	if jobs.Zone != "Central (CST)" || len(jobs.Errors) != 0 {
		t.Fatalf("zone %q errors %v", jobs.Zone, jobs.Errors)
	}
	if got := names(jobs); !slices.Equal(got, []string{"Backlog", "Digest", "From a chat", "Sign-in check", "Weekly review"}) {
		t.Fatalf("jobs = %v (the draft must not show)", got)
	}
	a1 := find(t, jobs, "Weekly review")
	if a1.State != hub.JobRunning || a1.Next != "Tue Nov 17, 1:00 AM" || a1.NextRel != "in 6 days" ||
		a1.Expr != "0 1 * * 2" || a1.RunsIn != "Sandbox, Firefox on upstairs-pc" ||
		a1.ModelLabel != "Follows the chat model (qwen-big)" || a1.ResultsText != "Telegram, General topic" {
		t.Fatalf("a1 = %+v", a1)
	}
	if len(a1.Runs) != 2 || !a1.Runs[0].Running || a1.Runs[0].SessionID != "s_new" || a1.Runs[1].Duration != "2m 41s" ||
		a1.Runs[1].Result != "Finished" || a1.Runs[1].Started != "Tue Nov 10, 6:00 PM" {
		t.Fatalf("a1 runs = %+v", a1.Runs)
	}
	for _, r := range a1.Runs {
		if strings.Contains(r.Result, "secret") {
			t.Fatal("a run row carries the chat's title")
		}
	}
	checkStates(t, jobs)
}

// checkStates checks the states, flags and labels of the jobs of
// TestJobsShowStateTimesAndRuns.
func checkStates(t *testing.T, jobs hub.Jobs) {
	t.Helper()
	if a2 := find(t, jobs, "Sign-in check"); a2.State != hub.JobWarn || a2.Flag != "Needs login" || a2.ModelLabel != "qwen-small (pinned)" || a2.RunsIn != sandbox {
		t.Fatalf("a2 = %+v", a2)
	}
	a3 := find(t, jobs, "Digest")
	if a3.State != hub.JobFailed || a3.Flag != "Last run failed" || !a3.Runs[0].Failed || a3.Runs[0].Result != "Failed: gh: API rate limit exceeded" ||
		a3.Last != a3.Runs[0].Result {
		t.Fatalf("a3 = %+v", a3)
	}
	if a4 := find(t, jobs, "Backlog"); a4.State != hub.JobPaused || a4.Flag != "Paused" || a4.Next != "" {
		t.Fatalf("a4 = %+v", a4)
	}
	if a6 := find(t, jobs, "From a chat"); !a6.Locked || a6.Results != hub.ResultsOrigin {
		t.Fatalf("a6 = %+v", a6)
	}
	if jobs.Jobs[len(jobs.Jobs)-1].NextAt != "" {
		t.Fatal("jobs without a next run must come last")
	}
}

func TestOnlyAllowedUsersReachJobs(t *testing.T) {
	t.Parallel()
	e := setup(t, alice)
	ctx := context.Background()
	e.dash.Add(map[string]any{"id": "j1", kName: "Mine"})
	calls := e.dash.Count("")
	if _, err := e.svc.JobOptions(ctx, carol, backend); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("options = %v", err)
	}
	if _, err := e.svc.PauseJob(ctx, carol, backend, "j1"); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("pause = %v", err)
	}
	if err := e.svc.DeleteJob(ctx, carol, "other", "j1"); !errors.Is(err, hub.ErrUnknownBackend) {
		t.Fatalf("other backend = %v", err)
	}
	if jobs, _ := e.svc.Jobs(ctx, carol); len(jobs.Jobs) != 0 {
		t.Fatalf("carol sees %v", names(jobs))
	}
	if e.dash.Count("") != calls {
		t.Fatalf("a refused request reached Hermes: %v", e.dash.Calls[calls:])
	}
}

func TestSharedBackendShowsOnlyOwnJobs(t *testing.T) {
	t.Parallel()
	e := setup(t, alice, bob)
	ctx := context.Background()
	e.dash.Add(map[string]any{"id": "ja", kName: "Alice's"})
	e.dash.Add(map[string]any{"id": "jb", kName: "Bob's", kOwner: bob})
	e.dash.Add(map[string]any{"id": "jx", kName: "Nobody's"})
	// talaria's job_owners wins over what the hub recorded.
	e.dash.Add(map[string]any{"id": "jc", kName: claimed, kOwner: alice})
	e.choices(t, map[string]string{"ja": devBob}, map[string]string{"ja": alice, "jc": bob})

	if got, _ := e.svc.Jobs(ctx, alice); !slices.Equal(names(got), []string{"Alice's"}) {
		t.Fatalf("alice sees %v", names(got))
	} else if got.Jobs[0].RunsIn != "Sandbox, Firefox on another person's computer" {
		t.Fatalf("runs in = %q", got.Jobs[0].RunsIn)
	}
	if got, _ := e.svc.Jobs(ctx, bob); !slices.Equal(names(got), []string{"Bob's", claimed}) {
		t.Fatalf("bob sees %v", names(got))
	}
	for _, id := range []string{"ja", "jx"} {
		if _, err := e.svc.PauseJob(ctx, bob, backend, id); !errors.Is(err, hub.ErrUnknownJob) {
			t.Fatalf("bob pauses %s: %v", id, err)
		}
		if err := e.svc.DeleteJob(ctx, bob, backend, id); !errors.Is(err, hub.ErrUnknownJob) {
			t.Fatalf("bob deletes %s: %v", id, err)
		}
		if err := e.svc.RunJob(ctx, bob, backend, id); !errors.Is(err, hub.ErrUnknownJob) {
			t.Fatalf("bob runs %s: %v", id, err)
		}
		if _, err := e.svc.JobRuns(ctx, bob, backend, id); !errors.Is(err, hub.ErrUnknownJob) {
			t.Fatalf("bob reads runs of %s: %v", id, err)
		}
	}
	if e.dash.Count("POST /api/cron/jobs/ja") != 0 || e.dash.Count("DELETE") != 0 {
		t.Fatalf("a refused change reached Hermes: %v", e.dash.Calls)
	}
	// Hermes also finds a job by its name; the hub acts only on ids.
	if _, err := e.svc.PauseJob(ctx, alice, backend, "Alice's"); !errors.Is(err, hub.ErrUnknownJob) {
		t.Fatalf("pause by name = %v", err)
	}
	// Without browser.json only the hub's record counts.
	e.gw.SetFile(scheduledtest.Home+"/device/browser.json", []byte("{"))
	if got, _ := e.svc.Jobs(ctx, alice); !slices.Equal(names(got), []string{claimed}) {
		t.Fatalf("alice sees %v without browser.json", names(got))
	}
}

func str(s string) *string { return &s }

func TestUpdateSendsOnlyAgentFields(t *testing.T) {
	t.Parallel()
	e := setup(t, alice)
	ctx := context.Background()
	e.dash.Add(map[string]any{"id": "j1", kName: "Old", kDeliver: "origin"})
	j, err := e.svc.UpdateJob(ctx, alice, backend, "j1", hub.JobInput{Name: str("  New  "), Prompt: str("Report the queue"),
		Model: &hub.JobModel{Provider: custom, Model: small}, Results: str(hub.ResultsHub)})
	if err != nil {
		t.Fatal(err)
	}
	if j.Name != "New" || j.ResultsText != "This hub only" || j.ModelLabel != "qwen-small (pinned)" {
		t.Fatalf("job = %+v", j)
	}
	up, ok := e.dash.Bodies[slices.Index(e.dash.Calls, "PUT /api/cron/jobs/j1")]["updates"].(map[string]any)
	if !ok {
		t.Fatal("no updates")
	}
	keys := make([]string, 0, len(up))
	for k := range up {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"deliver", "model", "name", "prompt", "provider", "talaria_results"}) || up["deliver"] != "local" {
		t.Fatalf("updates = %v", up)
	}
	for _, in := range []hub.JobInput{
		{Results: str("everywhere")},
		{Name: str("   ")},
		{Prompt: str(strings.Repeat("x", 20001))},
		{Model: &hub.JobModel{Model: small}},
	} {
		if _, err := e.svc.UpdateJob(ctx, alice, backend, "j1", in); !errors.Is(err, hub.ErrBadValue) {
			t.Fatalf("update %+v = %v", in, err)
		}
	}
	// Following the chat model clears the pin.
	if j, err = e.svc.UpdateJob(ctx, alice, backend, "j1", hub.JobInput{Model: &hub.JobModel{}}); err != nil || j.Model.Model != "" {
		t.Fatalf("unpin = %+v %v", j.Model, err)
	}
	// Hermes's reason for a schedule it cannot read reaches the page.
	if _, err := e.svc.UpdateJob(ctx, alice, backend, "j1", hub.JobInput{Schedule: str("whenever")}); !errors.Is(err, hub.ErrBadValue) ||
		!strings.Contains(err.Error(), "Invalid schedule") {
		t.Fatalf("bad schedule = %v", err)
	}
}

func TestJobsThatRunCodeOnTheServerStayLocked(t *testing.T) {
	t.Parallel()
	e := setup(t, alice)
	ctx := context.Background()
	for _, f := range []map[string]any{{kScript: "check.py"}, {"no_agent": true}, {"workdir": "/srv"}, {"monitor_url": "http://x"}} {
		id := fmt.Sprint(e.dash.Add(f)["id"])
		if _, err := e.svc.UpdateJob(ctx, alice, backend, id, hub.JobInput{Name: str("x")}); !errors.Is(err, hub.ErrJobLocked) {
			t.Fatalf("update %v = %v", f, err)
		}
		if _, err := e.svc.ResumeJob(ctx, alice, backend, id); !errors.Is(err, hub.ErrJobLocked) {
			t.Fatalf("resume %v = %v", f, err)
		}
		if err := e.svc.RunJob(ctx, alice, backend, id); !errors.Is(err, hub.ErrJobLocked) {
			t.Fatalf("run %v = %v", f, err)
		}
		if _, err := e.svc.PauseJob(ctx, alice, backend, id); err != nil {
			t.Fatalf("pause %v = %v", f, err)
		}
		if err := e.svc.DeleteJob(ctx, alice, backend, id); err != nil {
			t.Fatalf("delete %v = %v", f, err)
		}
	}
	if e.dash.Count("PUT") != 0 || e.dash.Triggered != 0 || e.dash.Count("POST /api/cron/jobs/job001/resume") != 0 {
		t.Fatalf("a locked change reached Hermes: %v", e.dash.Calls)
	}
}

func TestCreateMakesItPausedThenResumes(t *testing.T) {
	t.Parallel()
	e := setup(t, alice)
	ctx := context.Background()
	j, err := e.svc.CreateJob(ctx, alice, backend, hub.JobInput{Name: str("Digest"), Schedule: str("0 9 * * *"), Prompt: str("Summarize")})
	if err != nil {
		t.Fatal(err)
	}
	if j.State != hub.JobOK || j.Next != "Thu Nov 12, 9:00 AM" || j.ResultsText != "Telegram, General topic" {
		t.Fatalf("job = %+v", j)
	}
	create := e.dash.Bodies[slices.Index(e.dash.Calls, "POST /api/cron/jobs")]
	if create[paused] != true || create["deliver"] != "local" {
		t.Fatalf("create = %v", create)
	}
	for _, k := range []string{"script", "workdir", "no_agent", "base_url", "enabled_toolsets", "context_from"} {
		if _, ok := create[k]; ok {
			t.Fatalf("create sent %s", k)
		}
	}
	rec := e.dash.Job(j.ID)
	if rec[kOwner] != alice || rec["talaria_results"] != hub.ResultsGeneral || rec[kEnabled] != true || rec["paused_reason"] != nil {
		t.Fatalf("record = %v", rec)
	}
	// A schedule Hermes cannot read makes nothing.
	before := len(e.dash.Jobs)
	if _, err := e.svc.CreateJob(ctx, alice, backend, hub.JobInput{Name: str("x"), Schedule: str("someday"), Prompt: str("y")}); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("bad schedule = %v", err)
	}
	if _, err := e.svc.CreateJob(ctx, alice, backend, hub.JobInput{Name: str("x"), Schedule: str("0 9 * * *")}); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("no prompt = %v", err)
	}
	if len(e.dash.Jobs) != before {
		t.Fatalf("jobs = %d, want %d", len(e.dash.Jobs), before)
	}
}

func TestCreateRemovesAJobItCannotResume(t *testing.T) {
	t.Parallel()
	e := setup(t, alice)
	ctx := context.Background()
	// The fake's clock moves on between the parse and the resume, so the
	// one-shot is in the past by then.
	clock := e.now
	e.dash.Now = func() time.Time { clock = clock.Add(10 * time.Minute); return clock }
	if _, err := e.svc.CreateJob(ctx, alice, backend, hub.JobInput{Name: str("x"), Schedule: str("in 1m"), Prompt: str("y")}); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("create = %v", err)
	}
	if len(e.dash.Jobs) != 0 {
		t.Fatalf("left %v", e.dash.Jobs)
	}
}

func TestPreviewUsesOneDraftAndCreateTakesIt(t *testing.T) {
	t.Parallel()
	e := setup(t, alice, bob)
	ctx := context.Background()
	p, err := e.svc.PreviewSchedule(ctx, alice, backend, "every monday 9am", "")
	if err != nil || !p.OK || p.Draft == "" || p.Expr != "0 9 * * 1" ||
		!slices.Equal(p.Next, []string{"Mon Nov 16, 9:00 AM", "Mon Nov 23, 9:00 AM"}) {
		t.Fatalf("preview = %+v %v", p, err)
	}
	draft := p.Draft
	if rec := e.dash.Job(draft); rec[kEnabled] != false || rec[kOwner] != alice {
		t.Fatalf("draft = %v", rec)
	}
	p, err = e.svc.PreviewSchedule(ctx, alice, backend, "not a schedule", draft)
	if err != nil || p.OK || !strings.Contains(p.Error, "Invalid schedule") || p.Draft != draft {
		t.Fatalf("bad preview = %+v %v", p, err)
	}
	p, err = e.svc.PreviewSchedule(ctx, alice, backend, "every 6h", draft)
	if err != nil || !p.OK || p.Draft != draft || !slices.Equal(p.Next, []string{"Thu Nov 12, 2:00 AM", "Thu Nov 12, 8:00 AM"}) {
		t.Fatalf("interval preview = %+v %v", p, err)
	}
	if e.dash.Count("POST /api/cron/jobs") != 1 || len(e.dash.Jobs) != 1 {
		t.Fatalf("drafts = %d", len(e.dash.Jobs))
	}
	// Drafts never show, and another user cannot use or remove them.
	if jobs, _ := e.svc.Jobs(ctx, alice); len(jobs.Jobs) != 0 {
		t.Fatalf("draft shows: %v", names(jobs))
	}
	if p, _ := e.svc.PreviewSchedule(ctx, bob, backend, "every 2h", draft); p.Draft == draft {
		t.Fatal("bob reused alice's draft")
	}
	if err := e.svc.DiscardDraft(ctx, bob, backend, draft); err != nil || e.dash.Job(draft) == nil {
		t.Fatalf("bob discarded alice's draft: %v", err)
	}
	j, err := e.svc.CreateJob(ctx, alice, backend, hub.JobInput{Name: str("Six"), Schedule: str("every 6h"), Prompt: str("p"), Draft: draft,
		Results: str(hub.ResultsTopic)})
	if err != nil || j.ID != draft || j.State != hub.JobOK || j.ResultsText != "Telegram, its own topic" {
		t.Fatalf("create = %+v %v", j, err)
	}
	if rec := e.dash.Job(draft); rec["paused_reason"] != nil || rec[kName] != "Six" {
		t.Fatalf("record = %v", rec)
	}
}

func TestRunNowKeepsAPausedJobPaused(t *testing.T) {
	t.Parallel()
	e := setup(t, alice)
	ctx := context.Background()
	e.dash.Add(map[string]any{"id": "p1", kEnabled: false, kState: paused, kNext: nil})
	e.dash.Hold = make(chan struct{})
	if err := e.svc.RunJob(ctx, alice, backend, "p1"); err != nil {
		t.Fatal(err)
	}
	<-e.dash.Activated
	deadline := time.Now().Add(5 * time.Second)
	for e.dash.Job("p1")[kState] != paused {
		if time.Now().After(deadline) {
			t.Fatalf("job = %v", e.dash.Job("p1"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ok, err := e.svc.CancelJob(ctx, alice, backend, "p1"); err != nil || !ok {
		t.Fatalf("cancel = %v %v", ok, err)
	}
	close(e.dash.Hold)
}

func TestRunNowAnswersAQuickRefusal(t *testing.T) {
	t.Parallel()
	e := setup(t, alice)
	ctx := context.Background()
	e.dash.Add(map[string]any{"id": "r1"})
	if err := e.svc.RunJob(ctx, alice, backend, "r1"); err != nil {
		t.Fatal(err)
	}
	if e.dash.Triggered != 1 {
		t.Fatalf("triggered = %d", e.dash.Triggered)
	}
	if err := e.svc.RunJob(ctx, alice, backend, "missing"); !errors.Is(err, hub.ErrUnknownJob) {
		t.Fatalf("missing = %v", err)
	}
}

func TestOptionsListSignedInModels(t *testing.T) {
	t.Parallel()
	e := setup(t, alice)
	o, err := e.svc.JobOptions(context.Background(), alice, backend)
	if err != nil {
		t.Fatal(err)
	}
	if o.DefaultModel != "qwen-big" || o.RunsIn != sandbox || len(o.Providers) != 1 || o.Providers[0].ID != custom ||
		len(o.Providers[0].Models) != 2 {
		t.Fatalf("options = %+v", o)
	}
}
