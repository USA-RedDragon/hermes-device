package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/scheduled"
	"github.com/USA-RedDragon/hermes-hub/internal/scheduled/scheduledtest"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/web"
)

const providerBasic = "basic"

func sendJSON(t *testing.T, c *http.Client, method, u, csrf string, body any) (*http.Response, string) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = strings.NewReader(string(b))
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, u, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp, string(rb)
}

func TestScheduledAPI(t *testing.T) {
	t.Parallel()
	var e *hubEnv
	e = setupWith(t, func(cfg *config.Config, opts *web.Options, _ *store.Store) {
		d := gateway.NewDialer(gateway.Credentials{BaseURL: cfg.Backends[0].URL, Provider: providerBasic, Username: clientID, Password: "pw"})
		opts.Scheduled = scheduled.New(scheduled.Options{Access: lateAccess{&e}, Backends: cfg.Backends,
			Requesters: map[string]scheduled.Requester{"pc": d}})
	})
	dash := scheduledtest.New(e.gw)
	dash.Add(map[string]any{"id": "j1", fieldName: "Digest"})
	dash.Add(map[string]any{"id": "j2", fieldName: "Script job", "script": "x.py"})
	base := e.url + "/api/scheduled/pc/jobs/"

	alice := client(t)
	m := login(t, e, alice)
	if _, body := get(t, alice, e.url+"/api/me"); !strings.Contains(body, `"scheduled":true`) {
		t.Fatalf("me = %s", body)
	}
	resp, body := get(t, alice, e.url+"/api/scheduled")
	var jobs hub.Jobs
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &jobs) != nil || len(jobs.Jobs) != 2 {
		t.Fatalf("jobs = %d %s", resp.StatusCode, body)
	}

	checkRefusals(t, e, alice, m.CSRF, base, dash)

	resp, body = sendJSON(t, alice, http.MethodPut, base+"j1", m.CSRF, map[string]string{fieldName: "Digest 2"})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"name":"Digest 2"`) {
		t.Fatalf("update = %d %s", resp.StatusCode, body)
	}
	if resp, body := sendJSON(t, alice, http.MethodPut, base+"j2", m.CSRF, map[string]string{fieldName: "x"}); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(body, "script") {
		t.Fatalf("locked update = %d %s", resp.StatusCode, body)
	}
	if resp, body := sendJSON(t, alice, http.MethodPut, base+"j1", m.CSRF, map[string]string{"schedule": "whenever"}); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body, "Invalid schedule") {
		t.Fatalf("bad schedule = %d %s", resp.StatusCode, body)
	}
	if resp, _ := post(t, alice, base+"j1/run", m.CSRF, e.url, map[string]string{}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run = %d", resp.StatusCode)
	}
	if resp, body := post(t, alice, base+"j1/pause", m.CSRF, e.url, map[string]string{}); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"state":"paused"`) {
		t.Fatalf("pause = %d %s", resp.StatusCode, body)
	}
	if resp, _ := post(t, alice, base+"j1/explode", m.CSRF, e.url, map[string]string{}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown verb = %d", resp.StatusCode)
	}
	if resp, _ := get(t, alice, base+"nope/runs"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing job runs = %d", resp.StatusCode)
	}
	resp, body = post(t, alice, e.url+"/api/scheduled/pc/preview", m.CSRF, e.url, map[string]string{"schedule": "every monday 9am"})
	var p hub.SchedulePreview
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &p) != nil || !p.OK || p.Draft == "" {
		t.Fatalf("preview = %d %s", resp.StatusCode, body)
	}
	if resp, _ := sendJSON(t, alice, http.MethodDelete, e.url+"/api/scheduled/pc/drafts/"+p.Draft, m.CSRF, nil); resp.StatusCode != http.StatusOK ||
		dash.Job(p.Draft) != nil {
		t.Fatalf("discard = %d", resp.StatusCode)
	}
	if resp, _ := sendJSON(t, alice, http.MethodDelete, base+"j1", m.CSRF, nil); resp.StatusCode != http.StatusOK || dash.Job("j1") != nil {
		t.Fatalf("delete = %d", resp.StatusCode)
	}

	checkOtherUser(t, e, base)
}

// checkOtherUser checks that bob, who may not use pc, cannot see or
// change its jobs.
func checkOtherUser(t *testing.T, e *hubEnv, base string) {
	t.Helper()
	bob := client(t)
	e.is.as("sub-bob", userBob)
	mb := login(t, e, bob)
	if _, body := get(t, bob, e.url+"/api/scheduled"); strings.Contains(body, "Script job") {
		t.Fatalf("bob sees alice's jobs: %s", body)
	}
	if resp, _ := post(t, bob, base+"j2/pause", mb.CSRF, e.url, map[string]string{}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob pause = %d", resp.StatusCode)
	}
}

// checkRefusals checks that changes without the CSRF token or from
// another origin never reach Hermes.
func checkRefusals(t *testing.T, e *hubEnv, alice *http.Client, csrf, base string, dash *scheduledtest.Dash) {
	t.Helper()
	if resp, _ := sendJSON(t, alice, http.MethodPut, base+"j1", "", map[string]string{fieldName: "x"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("update without CSRF = %d", resp.StatusCode)
	}
	if resp, _ := sendJSON(t, alice, http.MethodDelete, base+"j1", "", nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete without CSRF = %d", resp.StatusCode)
	}
	if resp, _ := post(t, alice, base+"j1/run", "", e.url, map[string]string{}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("run without CSRF = %d", resp.StatusCode)
	}
	if resp, _ := post(t, alice, base+"j1/pause", csrf, "https://evil.example", map[string]string{}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("pause cross origin = %d", resp.StatusCode)
	}
	if dash.Count("PUT") != 0 || dash.Count("DELETE") != 0 || dash.Count("POST") != 0 {
		t.Fatalf("a refused change reached Hermes: %v", dash.Calls)
	}
}
