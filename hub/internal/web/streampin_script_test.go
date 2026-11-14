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

// A reply that streams into a long chat keeps the view at the bottom of
// the log when the reader is there, leaves it alone when the reader
// scrolled up, and ends up drawn as a full render of the final text draws
// it. testdata/streampin/run.js drives the real page in headless Chrome.
func TestStreamingKeepsViewPinnedChrome(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	ms := make([]map[string]any, 0, 240)
	for i := range 60 {
		ms = append(ms, map[string]any{kRole: roleUser, kText: fmt.Sprintf("question %d", i)},
			map[string]any{kRole: roleAssistant, kText: strings.Repeat(fmt.Sprintf("Answer %d fills the log. ", i), 12)})
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "sp", Title: "Streaming", Live: true, StartedAt: 900, Messages: ms})
	prof, err := os.MkdirTemp("", "streampin-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	signal := filepath.Join(prof, "go")
	var full strings.Builder
	delta := func(i int) string {
		switch {
		case i%60 == 59:
			return "\n\n"
		case i%97 == 40:
			return "\n\n- item one\n- item two\n- item three\n\n"
		case i%131 == 70:
			return "\n\n```go\nfmt.Println(1)\n\nfmt.Println(2)\n```\n\n"
		case i%23 == 5:
			return "**bold** "
		}
		return fmt.Sprintf("tok%d ", i)
	}
	go func() {
		for {
			select {
			case <-t.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			if _, err := os.Stat(signal); err == nil {
				break
			}
		}
		e.gw.SetRunning("sp", true)
		e.gw.Publish("sp", hub.KindMessageStart, map[string]any{})
		send := func(from, to int) {
			for i := from; i < to; i++ {
				d := delta(i)
				full.WriteString(d)
				e.gw.Publish("sp", hub.KindMessageDelta, map[string]any{kText: d})
				time.Sleep(10 * time.Millisecond)
			}
		}
		send(0, 150)
		time.Sleep(900 * time.Millisecond)
		send(150, 400)
		time.Sleep(900 * time.Millisecond)
		send(400, 500)
		full.WriteString(" final-end")
		e.gw.Publish("sp", hub.KindMessageDelta, map[string]any{kText: " final-end"})
		e.gw.FinishReply("sp", full.String())
	}()
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "streampin", "run.js"), chrome, e.url, prof, signal).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
