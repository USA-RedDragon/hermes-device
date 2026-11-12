package web_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
)

func optr[T any](v T) *T { return &v }

const (
	activityBase  = "/api/sessions/pc/s1"
	steerA        = "/subagents/sa-0-a/steer"
	statusRunning = "running"
)

// activityEnv gives s1 a subagent and a background command, and another
// chat of the same backend its own.
func activityEnv(t *testing.T) *hubEnv {
	t.Helper()
	e := setup(t)
	e.gw.AddSession(&gatewaytest.Session{ID: "s1", Title: "First", Live: true, StartedAt: 100,
		Subagents: []gateway.SubagentRow{{SubagentID: "sa-0-a", Goal: "Read the store", Status: optr(statusRunning), AcceptingSteer: optr(true)}},
		Processes: []gateway.ProcessRow{{SessionID: "proc_a", Command: "go test ./...", Status: statusRunning}}})
	e.gw.AddSession(&gatewaytest.Session{ID: "s2", Title: "Second", Live: true, StartedAt: 50,
		Subagents: []gateway.SubagentRow{{SubagentID: "sa-0-b", Goal: "another chat's", Status: optr(statusRunning), AcceptingSteer: optr(true)}},
		Processes: []gateway.ProcessRow{{SessionID: "proc_b", Command: "sleep 99", Status: statusRunning}}})
	return e
}

func TestActivityAPI(t *testing.T) {
	t.Parallel()
	e := activityEnv(t)
	c := client(t)
	m := login(t, e, c)
	resp, body := get(t, c, e.url+activityBase+"/activity")
	var st struct {
		Subagents []struct {
			ID string `json:"id"`
		} `json:"subagents"`
		Processes []struct {
			ID      string `json:"id"`
			Running bool   `json:"running"`
		} `json:"processes"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &st) != nil || len(st.Subagents) != 1 ||
		st.Subagents[0].ID != "sa-0-a" || len(st.Processes) != 1 || !st.Processes[0].Running || strings.Contains(body, "sa-0-b") {
		t.Fatalf("activity = %d %s", resp.StatusCode, body)
	}
	for _, tc := range []struct {
		path string
		body any
		want int
	}{
		{steerA, map[string]string{kText: "also check the docs"}, http.StatusAccepted},
		{"/subagents/sa-0-b/steer", map[string]string{kText: "hi"}, http.StatusNotFound},
		{steerA, map[string]string{kText: ""}, http.StatusBadRequest},
		{"/processes/proc_b/stop", map[string]string{}, http.StatusNotFound},
		{"/subagents/sa-0-b/stop", map[string]string{}, http.StatusNotFound},
		{"/processes/proc_a/stop", map[string]string{}, http.StatusAccepted},
		{"/subagents/sa-0-a/stop", map[string]string{}, http.StatusAccepted},
		{"/subagents-paused", map[string]bool{"paused": true}, http.StatusOK},
		{"/background", map[string]string{kText: "summarize"}, http.StatusAccepted},
	} {
		if resp, body := post(t, c, e.url+activityBase+tc.path, m.CSRF, "", tc.body); resp.StatusCode != tc.want {
			t.Errorf("%s = %d %s", tc.path, resp.StatusCode, body)
		}
	}
	if resp, _ := post(t, c, e.url+activityBase+"/processes/proc_a/stop", "", "", map[string]string{}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("stop without CSRF = %d", resp.StatusCode)
	}
	if s, _ := e.gw.Session("s2"); s.Processes[0].Status != "running" || len(s.Subagents) != 1 {
		t.Fatalf("another chat's work changed: %+v", s)
	}
	if got := e.gw.Steers(); len(got) != 1 || got[0].SessionID != "s1" {
		t.Fatalf("steers = %+v", got)
	}
}

func TestActivityAPIRefusesOtherUsers(t *testing.T) {
	t.Parallel()
	e := activityEnv(t)
	c := client(t)
	e.is.mu.Lock()
	e.is.user, e.is.groups = userBob, []string{groupFamily}
	e.is.mu.Unlock()
	m := login(t, e, c)
	if resp, _ := get(t, c, e.url+activityBase+"/activity"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob activity on alice's backend = %d", resp.StatusCode)
	}
	for _, path := range []string{steerA, "/processes/proc_a/stop", "/subagents/sa-0-a/stop", "/background", "/subagents-paused"} {
		if resp, _ := post(t, c, e.url+activityBase+path, m.CSRF, "", map[string]any{kText: "x", "paused": true}); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("bob %s on alice's backend = %d", path, resp.StatusCode)
		}
	}
	if resp, _ := get(t, c, e.url+activityBase+"/background-tokens?task=bg_x"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob background tokens = %d", resp.StatusCode)
	}
	if len(e.gw.Steers()) != 0 {
		t.Fatal("bob steered a subagent")
	}
}

func TestBackgroundToolAPI(t *testing.T) {
	t.Parallel()
	e := activityEnv(t)
	c := client(t)
	m := login(t, e, c)
	const path = activityBase + "/tools/call_1/background"
	if resp, body := post(t, c, e.url+path, m.CSRF, "", nil); resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("old Hermes = %d %s", resp.StatusCode, body)
	}
	e = activityEnv(t)
	e.gw.SetCapabilities(gateway.CapToolBackground)
	c = client(t)
	m = login(t, e, c)
	if resp, body := post(t, c, e.url+path, m.CSRF, "", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("idle chat = %d %s", resp.StatusCode, body)
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "s3", Title: "Third", Live: true, StartedAt: 30, Running: true})
	if resp, body := post(t, c, e.url+"/api/sessions/pc/s3/tools/call_1/background", m.CSRF, "", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("running chat = %d %s", resp.StatusCode, body)
	}
	if resp, _ := post(t, c, e.url+path, "", "", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("without CSRF = %d", resp.StatusCode)
	}
}
