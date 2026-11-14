package web_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// On a phone a finger that drags the log moves it a few pixels a frame. The
// reader's drag moves the view from its first move, while a reply streams
// in and when nothing does, and the view stays where the finger left it;
// dragging back to the newest message follows the reply again.
// testdata/touchscroll/run.js drives the real page in headless Chrome with
// mobile emulation and touch events from the DevTools protocol.
func TestTouchScrollMovesTheLogChrome(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	ms := make([]map[string]any, 0, 120)
	for i := range 60 {
		ms = append(ms, map[string]any{kRole: roleUser, kText: fmt.Sprintf("question %d", i)},
			map[string]any{kRole: roleAssistant, kText: strings.Repeat(fmt.Sprintf("Answer %d fills the log. ", i), 12)})
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "ts", Title: "Touch", Live: true, StartedAt: 900, Messages: ms})
	prof, err := os.MkdirTemp("", "touchscroll-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	start := filepath.Join(prof, "go")
	stop := filepath.Join(prof, "stop")
	seen := func(p string) bool { _, err := os.Stat(p); return err == nil }
	go func() {
		for !seen(start) {
			select {
			case <-t.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
		e.gw.SetRunning("ts", true)
		e.gw.Publish("ts", hub.KindMessageStart, map[string]any{})
		var full strings.Builder
		// A word every 30 ms, a new paragraph now and then, until the page
		// says it is done with the stream.
		for i := 0; !seen(stop) && i < 6000; i++ {
			select {
			case <-t.Context().Done():
				return
			case <-time.After(30 * time.Millisecond):
			}
			d := fmt.Sprintf("tok%d ", i)
			if i%40 == 39 {
				d = "\n\n"
			}
			full.WriteString(d)
			e.gw.Publish("ts", hub.KindMessageDelta, map[string]any{kText: d})
		}
		full.WriteString(" final-end")
		e.gw.Publish("ts", hub.KindMessageDelta, map[string]any{kText: " final-end"})
		e.gw.FinishReply("ts", full.String())
	}()
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "touchscroll", "run.js"), chrome, e.url, prof, start, stop).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
