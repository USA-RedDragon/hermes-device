package web_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

type stepsReply struct {
	Events []struct {
		Seq        int64           `json:"seq"`
		TS         string          `json:"ts"`
		Type       string          `json:"type"`
		CallID     string          `json:"call_id"`
		Name       string          `json:"name"`
		Args       json.RawMessage `json:"args"`
		Result     string          `json:"result"`
		ResultSize int             `json:"result_size"`
		DurationMS *int64          `json:"duration_ms"`
		Error      bool            `json:"error"`
		Text       string          `json:"text"`
	} `json:"events"`
	Epoch   int64 `json:"epoch"`
	Live    bool  `json:"live"`
	Running bool  `json:"running"`
	Details bool  `json:"details"`
}

const (
	eventsA      = activityBase + "/subagents/sa-0-a/events"
	goalRead     = "Read the store"
	kArgs        = "args"
	stepTool     = "tool"
	stepSteer    = "steer"
	kCommand     = "command"
	toolTerminal = "terminal"
	kTimestamp   = "timestamp"
)

func steps(t *testing.T, c *http.Client, u string) stepsReply {
	t.Helper()
	resp, body := get(t, c, u)
	var r stepsReply
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &r) != nil {
		t.Fatalf("GET %s = %d %s", u, resp.StatusCode, body)
	}
	return r
}

func kinds(r stepsReply) string {
	out := make([]string, 0, len(r.Events))
	for _, e := range r.Events {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

// firstSteps are a subagent's goal, a thought, a call with its result and
// a reply, as a Hermes with tool details stores them.
func firstSteps() []map[string]any {
	return []map[string]any{
		{kRole: roleUser, keyText: goalRead, kTimestamp: 1000.0},
		{kRole: roleAssistant, keyText: "", "reasoning": "I need the schema first.", kTimestamp: 1001.0,
			"tool_calls": []map[string]any{{"id": "c1", fieldName: toolTerminal, kArgs: map[string]any{kCommand: "ls store"}}}},
		{kRole: stepTool, fieldName: toolTerminal, "tool_call_id": "c1", kTimestamp: 1003.5, kArgs: map[string]any{kCommand: "ls store"},
			"result": `{"output": "", "exit_code": 2}`, "result_size": 30,
			"display_metadata": map[string]any{"tool_outcome": map[string]any{"duration_ms": 2500, "is_error": true}}},
		{kRole: roleAssistant, keyText: "The folder is missing.", kTimestamp: 1004.0},
	}
}

// The events of a subagent come typed, with seq and dated times, and polls
// after a seq return only the new steps.
func TestSubagentEventsAPI(t *testing.T) {
	t.Parallel()
	e := activityEnv(t)
	e.gw.SetCapabilities(gateway.CapTranscriptToolDetails)
	e.gw.AddChildRows("s1", "sa-0-a", firstSteps()...)
	c := client(t)
	login(t, e, c)
	r := steps(t, c, e.url+eventsA)
	if got := kinds(r); got != "goal,thinking,tool_start,tool_end,message" || !r.Live || !r.Running || !r.Details {
		t.Fatalf("events = %s %+v", got, r)
	}
	end := r.Events[3]
	if end.CallID != "c1" || end.Name != toolTerminal || end.DurationMS == nil || *end.DurationMS != 2500 || !end.Error ||
		end.Result != `{"output": "", "exit_code": 2}` || end.ResultSize != 30 || !strings.HasPrefix(end.TS, "1970-01-01T00:16:43.5") {
		t.Fatalf("tool end = %+v", end)
	}
	if string(r.Events[2].Args) != `{"command":"ls store"}` {
		t.Fatalf("args = %s", r.Events[2].Args)
	}
	for i, ev := range r.Events {
		if ev.Seq != int64(i+1) {
			t.Fatalf("seq %d = %d", i, ev.Seq)
		}
	}
	// Nothing new: an empty page. Then one more step.
	if again := steps(t, c, e.url+eventsA+"?after=5"); len(again.Events) != 0 {
		t.Fatalf("caught up = %+v", again)
	}
	e.gw.AddChildRows("s1", "sa-0-a", map[string]any{kRole: roleAssistant, keyText: "Done.", kTimestamp: 1010.0})
	if more := steps(t, c, e.url+eventsA+"?after=5"); kinds(more) != "message" || more.Events[0].Seq != 6 {
		t.Fatalf("after 5 = %+v", more)
	}
	if resp, _ := get(t, c, e.url+eventsA+"?after=x"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad after = %d", resp.StatusCode)
	}
}

// Steps stay after the subagent ended and Hermes no longer has it, and the
// messages the user sent are kept with their time.
func TestSubagentEventsKeptAfterTheEnd(t *testing.T) {
	t.Parallel()
	e := activityEnv(t)
	e.gw.SetCapabilities(gateway.CapTranscriptToolDetails)
	e.gw.AddChildRows("s1", "sa-0-a", firstSteps()...)
	c := client(t)
	m := login(t, e, c)
	steps(t, c, e.url+eventsA)
	if resp, body := post(t, c, e.url+activityBase+steerA, m.CSRF, "", map[string]string{kText: "Skip the docker build"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("steer = %d %s", resp.StatusCode, body)
	}
	r := steps(t, c, e.url+eventsA+"?after=5")
	if len(r.Events) != 1 || r.Events[0].Type != stepSteer || r.Events[0].Text != "Skip the docker build" || r.Events[0].TS == "" {
		t.Fatalf("steer event = %+v", r)
	}
	// The subagent ends and Hermes forgets its transcript (its parent
	// chat is gone from the gateway): the hub's copy answers.
	e.gw.EndSubagent("s1", "sa-0-a", "completed", "done")
	e.gw.Stop()
	r = steps(t, c, e.url+eventsA)
	if got := kinds(r); got != "goal,thinking,tool_start,tool_end,message,steer" || r.Live {
		t.Fatalf("stored events = %s live=%v", got, r.Live)
	}
}

// While Hermes restarts, the stored steps answer at once: the hub does not
// wait the restart out for a read its log can serve.
func TestSubagentEventsStoredWhileHermesRestarts(t *testing.T) {
	t.Parallel()
	e := activityEnv(t)
	e.gw.SetCapabilities(gateway.CapTranscriptToolDetails)
	e.gw.AddChildRows("s1", "sa-0-a", firstSteps()...)
	c := client(t)
	login(t, e, c)
	steps(t, c, e.url+eventsA)
	e.gw.BeginRestart()
	c.Timeout = 5 * time.Second
	r := steps(t, c, e.url+eventsA)
	if got := kinds(r); got != "goal,thinking,tool_start,tool_end,message" || r.Live {
		t.Fatalf("stored events = %s live=%v", got, r.Live)
	}
}

// A transcript Hermes rewrote (a compaction) starts a new epoch; the steers
// stay.
func TestSubagentEventsReset(t *testing.T) {
	t.Parallel()
	e := activityEnv(t)
	e.gw.SetCapabilities(gateway.CapTranscriptToolDetails)
	e.gw.AddChildRows("s1", "sa-0-a", firstSteps()...)
	c := client(t)
	m := login(t, e, c)
	first := steps(t, c, e.url+eventsA)
	post(t, c, e.url+activityBase+steerA, m.CSRF, "", map[string]string{kText: "keep going"})
	e.gw.RewriteChild("s1", "sa-0-a", map[string]any{kRole: roleUser, keyText: goalRead, kTimestamp: 1000.0},
		map[string]any{kRole: roleUser, keyText: "Summary of earlier steps", kTimestamp: 1005.0})
	r := steps(t, c, e.url+eventsA)
	if r.Epoch == first.Epoch || kinds(r) != "steer,goal,user" {
		t.Fatalf("after rewrite = %s epoch %d (was %d)", kinds(r), r.Epoch, first.Epoch)
	}
}

// A Hermes without tool details still gives text and calls, without
// results.
func TestSubagentEventsWithoutToolDetails(t *testing.T) {
	t.Parallel()
	e := activityEnv(t)
	e.gw.AddChildRows("s1", "sa-0-a", firstSteps()...)
	c := client(t)
	login(t, e, c)
	r := steps(t, c, e.url+eventsA)
	if got := kinds(r); got != "goal,tool_end,message" || r.Details || r.Events[1].Result != "" {
		t.Fatalf("events = %s %+v", got, r)
	}
}

// Only the chat's owner reads its subagents' steps: another user is
// refused the backend, and on a backend of their own never sees the
// owner's stored steers.
func TestSubagentEventsOwner(t *testing.T) {
	t.Parallel()
	e := activityEnv(t)
	e.gw.SetCapabilities(gateway.CapTranscriptToolDetails)
	e.gw.AddChildRows("s1", "sa-0-a", firstSteps()...)
	alice := client(t)
	m := login(t, e, alice)
	post(t, alice, e.url+activityBase+steerA, m.CSRF, "", map[string]string{kText: "private note to the subagent"})
	e.is.as("sub-bob", userBob, "family")
	bob := client(t)
	login(t, e, bob)
	if resp, body := get(t, bob, e.url+eventsA); resp.StatusCode != http.StatusForbidden || strings.Contains(body, goalRead) {
		t.Fatalf("bob on pc = %d %s", resp.StatusCode, body)
	}
	resp, body := get(t, bob, e.url+"/api/sessions/"+backendHome+"/s1/subagents/sa-0-a/events")
	if strings.Contains(body, "private note") {
		t.Fatalf("bob read alice's steer: %d %s", resp.StatusCode, body)
	}
	// Unknown subagents are empty, not another chat's.
	e.gw.AddSession(&gatewaytest.Session{ID: "s3", Title: "Other", Live: true})
	if r := steps(t, alice, e.url+"/api/sessions/pc/s3/subagents/sa-0-a/events"); len(r.Events) != 0 {
		t.Fatalf("s3 = %+v", r)
	}
}
