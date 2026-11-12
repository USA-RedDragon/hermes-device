package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
)

const (
	sOther   = "s-other"
	subMine  = "sa-0-mine"
	subOther = "sa-0-other"
	procMine = "proc_mine"
	procElse = "proc_else"
	// statusRunning is a running subagent's or process's status.
	statusRunning = "running"
)

func ptr[T any](v T) *T { return &v }

// activitySetup gives sLive subagents and a process, and another session
// of the same backend its own.
func activitySetup(t *testing.T) *env {
	t.Helper()
	e := setup(t, service.Options{})
	e.gw.AddSession(&gatewaytest.Session{ID: sOther, Live: true,
		Subagents: []gateway.SubagentRow{{SubagentID: subOther, Goal: "other", Status: ptr(statusRunning), AcceptingSteer: ptr(true)}},
		Processes: []gateway.ProcessRow{{SessionID: procElse, Command: "sleep 99", Status: statusRunning}}})
	e.gw.AddSession(&gatewaytest.Session{ID: sLive, Title: liveTitle, Live: true, StartedAt: 2000,
		Subagents: []gateway.SubagentRow{
			{SubagentID: subMine, Goal: "Read the store", Model: ptr("qwen"), Status: ptr(statusRunning), ToolCount: ptr(3),
				AcceptingSteer: ptr(true), StartedAt: ptr(1700000000.5)},
			{SubagentID: "sa-1-child", ParentID: ptr(subMine), Depth: ptr(1), Goal: "Child", Status: ptr(statusRunning),
				AcceptingSteer: ptr(false)},
		},
		Processes: []gateway.ProcessRow{{SessionID: procMine, Command: "go test ./...", Status: statusRunning, OutputTail: "ok a\n"}}})
	return e
}

func TestActivityListsOnlyTheSessionsWork(t *testing.T) {
	t.Parallel()
	e := activitySetup(t)
	e.gw.SetPaused(true)
	st, err := e.svc.Activity(context.Background(), userAlice, backendPC, sLive)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Subagents) != 2 || st.Subagents[0].ID != subMine || st.Subagents[0].ToolCount != 3 || st.Subagents[0].Model != "qwen" ||
		!st.Subagents[0].AcceptingSteer || st.Subagents[1].ParentID != subMine || st.Subagents[1].AcceptingSteer {
		t.Fatalf("subagents = %+v", st.Subagents)
	}
	if len(st.Processes) != 1 || st.Processes[0].ID != procMine || !st.Processes[0].Running || st.Processes[0].Output != "ok a\n" {
		t.Fatalf("processes = %+v", st.Processes)
	}
	if !st.Paused {
		t.Fatal("paused not passed on")
	}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["active"]; ok || strings.Contains(string(raw), subOther) {
		t.Fatalf("another chat's subagents leaked: %s", raw)
	}
	for _, c := range e.gw.Calls() {
		if c.Method == gateway.MethodSubagentList || c.Method == gateway.MethodProcessList {
			if c.Params["session_id"] != sLive || len(c.Params) != 1 {
				t.Fatalf("%s params = %v", c.Method, c.Params)
			}
		}
	}
}

func TestActivityRefusesOtherUsers(t *testing.T) {
	t.Parallel()
	e := activitySetup(t)
	ctx := context.Background()
	if _, err := e.svc.Activity(ctx, userBob, backendPC, sLive); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("activity = %v", err)
	}
	if err := e.svc.SteerSubagent(ctx, userBob, backendPC, sLive, subMine, "hi"); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("steer = %v", err)
	}
	if err := e.svc.StopProcess(ctx, userBob, backendPC, sLive, procMine); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("kill = %v", err)
	}
	if _, err := e.svc.Background(ctx, userBob, backendPC, sLive, "x"); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("background = %v", err)
	}
	if _, err := e.svc.PauseSubagents(ctx, userBob, backendPC, sLive, true); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("pause = %v", err)
	}
}

func TestSteerAndStopActOnlyOnTheSessionsSubagents(t *testing.T) {
	t.Parallel()
	e := activitySetup(t)
	ctx := context.Background()
	if err := e.svc.SteerSubagent(ctx, userAlice, backendPC, sLive, subMine, "  also add a test  "); err != nil {
		t.Fatal(err)
	}
	if got := e.gw.Steers(); len(got) != 1 || got[0] != (gatewaytest.Steer{SessionID: sLive, SubagentID: subMine, Text: "also add a test"}) {
		t.Fatalf("steers = %+v", got)
	}
	if err := e.svc.SteerSubagent(ctx, userAlice, backendPC, sLive, subOther, "hi"); !errors.Is(err, hub.ErrUnknownSubagent) {
		t.Fatalf("steer of another chat's subagent = %v", err)
	}
	if err := e.svc.SteerSubagent(ctx, userAlice, backendPC, sLive, "sa-1-child", "hi"); !errors.Is(err, hub.ErrSteerRejected) {
		t.Fatalf("steer of a subagent that takes none = %v", err)
	}
	if err := e.svc.SteerSubagent(ctx, userAlice, backendPC, sLive, subMine, "   "); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("empty steer = %v", err)
	}
	if len(e.gw.Steers()) != 1 {
		t.Fatalf("steers = %+v", e.gw.Steers())
	}
	if err := e.svc.StopSubagent(ctx, userAlice, backendPC, sLive, subOther); !errors.Is(err, hub.ErrUnknownSubagent) {
		t.Fatalf("stop of another chat's subagent = %v", err)
	}
	if s, _ := e.gw.Session(sOther); len(s.Subagents) != 1 {
		t.Fatal("another chat's subagent was stopped")
	}
	if err := e.svc.StopSubagent(ctx, userAlice, backendPC, sLive, subMine); err != nil {
		t.Fatal(err)
	}
	tail, err := e.svc.SubagentTail(ctx, userAlice, backendPC, sLive, "sa-1-child")
	if err != nil || !tail.Available || tail.Text == "" {
		t.Fatalf("tail = %+v, %v", tail, err)
	}
	if _, err := e.svc.SubagentTail(ctx, userAlice, backendPC, sLive, subOther); !errors.Is(err, hub.ErrUnknownSubagent) {
		t.Fatalf("tail of another chat's subagent = %v", err)
	}
}

func TestStopProcessKillsOnlyTheSessionsOwn(t *testing.T) {
	t.Parallel()
	e := activitySetup(t)
	ctx := context.Background()
	if err := e.svc.StopProcess(ctx, userAlice, backendPC, sLive, procElse); !errors.Is(err, hub.ErrUnknownProcess) {
		t.Fatalf("kill of another chat's process = %v", err)
	}
	for _, c := range e.gw.Calls() {
		if c.Method == gateway.MethodProcessKill {
			t.Fatalf("process.kill sent for another chat's process: %v", c.Params)
		}
	}
	if err := e.svc.StopProcess(ctx, userAlice, backendPC, sLive, procMine); err != nil {
		t.Fatal(err)
	}
	s, _ := e.gw.Session(sLive)
	if s.Processes[0].Status != "exited" {
		t.Fatalf("process = %+v", s.Processes[0])
	}
	if o, _ := e.gw.Session(sOther); o.Processes[0].Status != "running" {
		t.Fatal("another chat's process was killed")
	}
}

func TestPauseSubagentsNamesTheSession(t *testing.T) {
	t.Parallel()
	e := activitySetup(t)
	paused, err := e.svc.PauseSubagents(context.Background(), userAlice, backendPC, sLive, true)
	if err != nil || !paused {
		t.Fatalf("pause = %v, %v", paused, err)
	}
	for _, c := range e.gw.Calls() {
		if c.Method == gateway.MethodDelegationPause && (c.Params["session_id"] != sLive || c.Params["paused"] != true) {
			t.Fatalf("delegation.pause params = %v", c.Params)
		}
	}
}

func TestBackgroundPromptReachesViewers(t *testing.T) {
	t.Parallel()
	e := activitySetup(t)
	ctx := context.Background()
	ch, stop, err := e.svc.Attach(ctx, userAlice, backendPC, sLive, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	next(t, ch)
	if _, err := e.svc.Background(ctx, userAlice, backendPC, sLive, " "); !errors.Is(err, hub.ErrBadValue) {
		t.Fatalf("empty prompt = %v", err)
	}
	id, err := e.svc.Background(ctx, userAlice, backendPC, sLive, "summarize the PRs")
	if err != nil || len(id) < 4 || id[:3] != "bg_" {
		t.Fatalf("background = %q, %v", id, err)
	}
	// The answer may come before the hub's background.start: the fake
	// answers at once.
	var start, done map[string]any
	for start == nil || done == nil {
		ev := next(t, ch)
		switch ev.Kind {
		case service.BackgroundStartEvent:
			_ = json.Unmarshal(ev.Data, &start)
		case gateway.BackgroundCompleteEvent:
			_ = json.Unmarshal(ev.Data, &done)
		}
	}
	if start["task_id"] != id || start["text"] != "summarize the PRs" || done["task_id"] != id {
		t.Fatalf("start = %v, done = %v", start, done)
	}
}

func TestSessionsCountActivity(t *testing.T) {
	t.Parallel()
	e := activitySetup(t)
	ss, err := e.svc.Sessions(context.Background(), userAlice)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, s := range ss {
		got[s.ID] = s.Activity
	}
	if got[sLive] != 3 || got[sOther] != 2 || got["s-old"] != 0 {
		t.Fatalf("activity = %v", got)
	}
}

func TestSessionExists(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	ctx := context.Background()
	if ok, err := e.svc.SessionExists(ctx, userAlice, backendPC, "s-old"); !ok || err != nil {
		t.Fatalf("stored session = %v, %v", ok, err)
	}
	if ok, err := e.svc.SessionExists(ctx, userAlice, backendPC, "s-deleted"); ok || err != nil {
		t.Fatalf("deleted session = %v, %v", ok, err)
	}
	if _, err := e.svc.SessionExists(ctx, userBob, backendPC, "s-old"); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("bob = %v", err)
	}
	for _, c := range e.gw.Calls() {
		if c.Method == gateway.MethodSessionResume {
			t.Fatal("SessionExists subscribed to a session")
		}
	}
}

func TestBackgroundToolOnlyWhereHermesCan(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	e.gw.AddSession(&gatewaytest.Session{ID: sLive, Title: liveTitle, Live: true, StartedAt: 2000, Running: true, Generation: 4})
	ctx := context.Background()
	if err := e.svc.BackgroundTool(ctx, userAlice, backendPC, sLive, "call_1"); !errors.Is(err, hub.ErrNoToolBackground) {
		t.Fatalf("background tool on an old Hermes = %v", err)
	}
	for _, c := range e.gw.Calls() {
		if c.Method == gateway.MethodBackgroundTool {
			t.Fatalf("%s sent to a Hermes that does not list %s", c.Method, gateway.CapToolBackground)
		}
	}
	if err := e.svc.BackgroundTool(ctx, userBob, backendPC, sLive, "call_1"); !errors.Is(err, hub.ErrForbidden) {
		t.Fatalf("another user = %v", err)
	}
}

func TestBackgroundToolAsksTheRunningTurn(t *testing.T) {
	t.Parallel()
	e := setup(t, service.Options{})
	e.gw.SetCapabilities(gateway.CapToolBackground)
	e.gw.AddSession(&gatewaytest.Session{ID: sLive, Title: liveTitle, Live: true, StartedAt: 2000, Running: true, Generation: 4})
	e.gw.AddSession(&gatewaytest.Session{ID: sOther, Live: true, StartedAt: 1000})
	ctx := context.Background()
	if err := e.svc.BackgroundTool(ctx, userAlice, backendPC, sLive, "call_1"); err != nil {
		t.Fatal(err)
	}
	var sent []map[string]any
	for _, c := range e.gw.Calls() {
		if c.Method == gateway.MethodBackgroundTool {
			sent = append(sent, c.Params)
		}
	}
	if len(sent) != 1 || sent[0]["session_id"] != sLive || sent[0]["tool_call_id"] != "call_1" || sent[0]["execution_generation"] != float64(4) {
		t.Fatalf("sent = %v", sent)
	}
	if err := e.svc.BackgroundTool(ctx, userAlice, backendPC, sOther, ""); !errors.Is(err, hub.ErrNoRunningTool) {
		t.Fatalf("idle session = %v", err)
	}
}

func TestSnapshotSaysWhetherToolsCanMove(t *testing.T) {
	t.Parallel()
	for _, capable := range []bool{false, true} {
		e := setup(t, service.Options{})
		if capable {
			e.gw.SetCapabilities(gateway.CapToolBackground)
		}
		e.gw.AddSession(&gatewaytest.Session{ID: sLive, Title: liveTitle, Live: true, StartedAt: 2000})
		ch, stop, err := e.svc.Attach(context.Background(), userAlice, backendPC, sLive, 0)
		if err != nil {
			t.Fatal(err)
		}
		var snap hub.Snapshot
		if err := json.Unmarshal(next(t, ch).Data, &snap); err != nil {
			t.Fatal(err)
		}
		stop()
		if snap.ToolBackground != capable {
			t.Fatalf("capable %v: snapshot tool_background = %v", capable, snap.ToolBackground)
		}
	}
}

func TestActivityWaitsOutARestartWaitRefusal(t *testing.T) {
	t.Parallel()
	e := activitySetup(t)
	e.gw.RefuseWhileDraining(gateway.MethodSubagentList, 2)
	e.gw.RefuseWhileDraining(gateway.MethodProcessList, 1)
	st, err := e.svc.Activity(context.Background(), userAlice, backendPC, sLive)
	if err != nil {
		t.Fatalf("activity during a restart wait: %v", err)
	}
	if len(st.Subagents) != 2 || len(st.Processes) != 1 {
		t.Fatalf("activity = %+v", st)
	}
}

func TestActivityWaitsOutARestartClose(t *testing.T) {
	t.Parallel()
	e := activitySetup(t)
	if _, err := e.svc.Activity(context.Background(), userAlice, backendPC, sLive); err != nil {
		t.Fatal(err)
	}
	e.gw.RestartConnections()
	st, err := e.svc.Activity(context.Background(), userAlice, backendPC, sLive)
	if err != nil {
		t.Fatalf("activity after a restart close: %v", err)
	}
	if len(st.Subagents) != 2 {
		t.Fatalf("subagents = %+v", st.Subagents)
	}
}

func TestActivityStillFailsOnARealRefusal(t *testing.T) {
	t.Parallel()
	e := activitySetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.svc.Activity(ctx, userAlice, backendPC, "s-missing"); err == nil {
		t.Fatal("an unknown session must still fail")
	}
}
