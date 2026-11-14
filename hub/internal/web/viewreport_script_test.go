package web_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

const (
	vrJump  = "jump"
	vrWheel = "wheel"
	vrChat  = "Long"
)

// lockedBuf is a log sink that the hub and the test may use together.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) lines() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, ln := range strings.Split(l.b.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(ln), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// A chat the page shows leaves its newest message with nobody touching it,
// long after it opened: the page sends one report, by fetch, and the hub
// logs it. A reader's wheel scroll sends none. Both also log the page load
// with the release. testdata/viewreport/run.js drives the real page in
// headless Chrome and Firefox; each skips where node or its browser is
// missing.
func TestViewReportReachesHubChrome(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{vrJump, vrWheel} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			viewReportRun(t, chromePath(t), mode)
		})
	}
}

func TestViewReportReachesHubFirefox(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{vrJump, vrWheel} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			viewReportRun(t, firefoxPath(t), mode)
		})
	}
}

func viewReportRun(t *testing.T, bin, mode string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	sink := &lockedBuf{}
	e := setupWith(t, func(_ *config.Config, o *web.Options, _ *store.Store) {
		o.Version = "v1.2.3"
		o.Logger = slog.New(slog.NewJSONHandler(sink, nil))
	})
	ms := make([]map[string]any, 0, 200)
	for i := range 100 {
		ms = append(ms, map[string]any{kRole: roleUser, kText: "question " + strings.Repeat("x", i%7)},
			map[string]any{kRole: roleAssistant, kText: strings.Repeat("an answer that fills the log. ", 3+i%11)})
	}
	e.gw.AddSession(&gatewaytest.Session{ID: longOne, Title: vrChat, Live: true, StartedAt: 900, Messages: ms})
	prof, err := os.MkdirTemp("", "viewreport-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "viewreport", "run.js"), bin, e.url, prof, mode).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var left, loaded int
	for _, m := range sink.lines() {
		switch m["msg"] {
		case "view left the newest message":
			left++
			rep, _ := m["report"].(string)
			if strings.Contains(rep, "csrf") || strings.Contains(rep, longOne) {
				t.Errorf("the logged report holds a token or a chat id: %s", rep)
			}
		case "page loaded":
			loaded++
			if rep, _ := m["report"].(string); !strings.Contains(rep, `"version":"v1.2.3"`) {
				t.Errorf("page loaded line has no release: %s", rep)
			}
		}
	}
	wantLeft := 0
	if mode == vrJump {
		wantLeft = 1
	}
	if left != wantLeft || loaded != 1 {
		t.Fatalf("hub logged %d view reports (want %d) and %d page loads (want 1)\n%s", left, wantLeft, loaded, out)
	}
}
