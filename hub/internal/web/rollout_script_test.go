package web_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

// A Hermes rollout does not show in the page: while the gateway answers
// nothing for 45 seconds, a page that is open, and one loaded fresh on the
// other replica, keep the chat list, the title, the model line and the
// messages; only past AwayNotice a short note says the agent is away; and
// a message sent meanwhile shows at once and reaches Hermes when it is
// back. testdata/rollout/run.js drives the real page in headless Chrome.
// ROLLOUT_SHOT=prefix also writes a screenshot every second, and
// ROLLOUT_SOFT=1 records every failed check instead of stopping. It skips
// where node or Chrome is missing, and with -short.
func TestRolloutKeepsThePage(t *testing.T) {
	if testing.Short() {
		t.Skip("takes a minute")
	}
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setupService(t, nil, func(o *service.Options) { o.AwayNotice = 10 * time.Second })
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Rollout chat", Live: true, StartedAt: 900, Model: modelBig, Provider: "llama",
		Messages: []map[string]any{
			{kRole: roleUser, kText: "first question"},
			{kRole: roleAssistant, kText: "first answer"},
			{kRole: roleUser, kText: "second question"},
			{kRole: roleAssistant, kText: "second answer"},
		}})
	other := e.replica(t)
	var (
		mu      sync.Mutex
		release func()
	)
	poke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/silence":
			release = e.gw.Silence()
		case "/other":
			e.front.to(other)
		case "/release":
			if release != nil {
				release()
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(poke.Close)
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		if release != nil {
			release()
		}
	})
	prof, err := os.MkdirTemp("", "rollout-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	cmd := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "rollout", "run.js"), chrome, e.url, prof, poke.URL)
	cmd.Env = append(os.Environ(), "SHOT_PREFIX="+os.Getenv("ROLLOUT_SHOT"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", out)
	s, _ := e.gw.Session("s9")
	found := false
	for _, m := range s.Messages {
		found = found || m[kText] == "echo: sent during the switch"
	}
	if !found || !strings.Contains(string(out), "ok") {
		t.Fatalf("the message sent during the switch did not reach Hermes: %+v\n%s", s.Messages, out)
	}
}
