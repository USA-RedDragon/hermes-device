package web_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

// After Hermes or the hub restarts, the page reconnects, and the chat it
// shows, and each chat opened next, stays at its newest message: a
// reconnect's snapshot, the hub's empty copy and the rows a turn replays
// must not leave the view on older messages. testdata/restartjump/run.js
// drives the real page in headless Chrome and Firefox. Each skips where
// node or its browser is missing.
func TestRestartKeepsNewestChrome(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"gw", "hub", "release", "away"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			restartJump(t, chromePath(t), mode, false)
		})
	}
}

// A tab that is hidden when the new release arrives reloads then; the
// reader comes back to the newest message. Chrome only: Firefox headless
// has no second tab to hide the first.
func TestRestartKeepsNewestHiddenChrome(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"hidden", "bggw", "bghub"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			restartJump(t, chromePath(t), mode, false)
		})
	}
}

func TestRestartKeepsNewestFirefox(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"gw", "hub", "release", "away"} {
		for _, running := range []bool{false, true} {
			if mode == "away" && running {
				continue
			}
			t.Run(fmt.Sprintf("%s_running_%v", mode, running), func(t *testing.T) {
				restartJump(t, firefoxPath(t), mode, running)
			})
		}
	}
}

func restartJump(t *testing.T, bin, mode string, running bool) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	e := setupWith(t, func(_ *config.Config, o *web.Options, _ *store.Store) { o.Version = "v1.0.0" })
	for k, c := range []struct {
		id    string
		turns int
	}{{"long1", 200}, {"long2", 300}, {"long3", 120}, {"long4", 80}} {
		ms := make([]map[string]any, 0, c.turns*4)
		for i := range c.turns {
			ms = append(ms, map[string]any{kRole: roleUser, kText: fmt.Sprintf("%s question %d", c.id, i)})
			if i%5 == 2 {
				call := fmt.Sprintf("c%d", i)
				ms = append(ms, map[string]any{kRole: roleAssistant, kText: "", kCalls: []map[string]any{{"id": call,
					fnKey: map[string]any{fieldName: toolTerminal, ilArgs: `{"command":"ls -la /tmp"}`}}}},
					map[string]any{kRole: stepTool, kCallID: call, fieldContent: strings.Repeat("line of output\n", 12)})
			}
			ms = append(ms, map[string]any{kRole: roleAssistant, kText: strings.Repeat(fmt.Sprintf("%s answer %d fills the log. ", c.id, i), 3+i%17)})
		}
		e.gw.AddSession(&gatewaytest.Session{ID: c.id, Title: "Long " + c.id, Live: true, StartedAt: float64(900 - k), Messages: ms})
	}
	if running {
		// The open chat and another one are mid-turn across the restart.
		go func() {
			tick := time.NewTicker(300 * time.Millisecond)
			defer tick.Stop()
			for n := 0; ; n++ {
				select {
				case <-t.Context().Done():
					return
				case <-tick.C:
				}
				id := []string{"long1", "long2"}[n%2]
				e.gw.SetRunning(id, true)
				e.gw.Publish(id, hub.KindMessageStart, map[string]any{})
				e.gw.Publish(id, hub.KindMessageDelta, map[string]any{kText: strings.Repeat("more words ", 40)})
			}
		}()
	}
	other := e.replica(t)
	if mode == "release" || mode == "hidden" {
		other = e.replicaAt(t, "v1.0.1")
	}
	poke := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/") {
		case "gwdown":
			e.gw.Stop()
			grow(e.gw)
		case "gwup":
			e.gw.FinishRestart()
		case "gw":
			e.gw.Stop()
			grow(e.gw)
			time.Sleep(1500 * time.Millisecond)
			e.gw.FinishRestart()
		case "hub", "release", "hidden":
			e.svc.Close()
			grow(e.gw)
			e.front.to(other)
			e.front.cut()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(poke.Close)
	prof, err := os.MkdirTemp("", "restartjump-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "restartjump", "run.js"), bin, e.url, prof, poke.URL, mode).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if os.Getenv("RESTARTJUMP_DEBUG") != "" {
		t.Logf("%s", out)
	}
}

// grow adds a turn to each long chat, as Hermes does while the hub is
// away or restarts: the hub's stored copy of each is one turn behind.
func grow(gw *gatewaytest.Gateway) {
	for _, id := range []string{"long1", "long2", "long3", "long4"} {
		s, ok := gw.Session(id)
		if !ok {
			continue
		}
		s.Messages = append(append([]map[string]any(nil), s.Messages...),
			map[string]any{kRole: roleUser, kText: id + " question after the restart"},
			map[string]any{kRole: roleAssistant, kText: id + " answer after the restart"})
		gw.AddSession(&s)
	}
}
