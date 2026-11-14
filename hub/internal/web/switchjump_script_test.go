package web_test

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
)

const (
	sjToolID = "tool_id"
	sjResult = "result"
)

// Switching between long chats at desktop size opens each at its newest
// message and keeps it there while the chat loads: the hub's held copy,
// the chat's own snapshot, its older rows, images that load late, a
// touchpad's scroll that goes on after the click, and a reload that put
// back another chat's place must not leave the view on older messages,
// and the page sends no report of a view that left its end.
// testdata/switchjump/run.js drives the real page with a slow gateway in
// headless Chrome and Firefox (SWITCHJUMP_N switches, SWITCHJUMP_SEED for
// their order). Each skips where node or its browser is missing.
func TestSwitchingChatsOpensAtNewestChrome(t *testing.T) {
	t.Parallel()
	switchJump(t, chromePath(t))
}

func TestSwitchingChatsOpensAtNewestFirefox(t *testing.T) {
	t.Parallel()
	switchJump(t, firefoxPath(t))
}

// sjChat is one chat of the switch test: its turns and how long Hermes
// takes to resume it.
type sjChat struct {
	id    string
	turns int
	slow  time.Duration
}

func switchJump(t *testing.T, bin string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	// The hub lets go of a chat nobody shows after a second, so a chat
	// opened again comes as the hub's held copy first, then Hermes' own.
	e := setupService(t, nil, func(o *service.Options) { o.IdleDetach = time.Second })
	img := image.NewRGBA(image.Rect(0, 0, 640, 420))
	for y := range 420 {
		for x := range 640 {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var pic bytes.Buffer
	if err := png.Encode(&pic, img); err != nil {
		t.Fatal(err)
	}
	const shots = "/opt/data/hermes/cache/screenshots/hermes-device/sj/"
	// Hermes answers a resume in 0.5 to 3 s, so the hub shows its held
	// copy first (after 0.8 s) and the chat's own snapshot later.
	chats := []sjChat{{"long1", 200, 500 * time.Millisecond}, {"long2", 450, 1500 * time.Millisecond},
		{"long3", 900, 3 * time.Second}, {"long4", 320, 1100 * time.Millisecond}}
	slow := map[string]time.Duration{}
	for k, c := range chats {
		slow[c.id] = c.slow
		ms := make([]map[string]any, 0, c.turns*4)
		for i := range c.turns {
			ms = append(ms, map[string]any{kRole: roleUser, kText: fmt.Sprintf("%s question %d", c.id, i)})
			if i%5 == 2 {
				call := fmt.Sprintf("c%d", i)
				ms = append(ms, map[string]any{kRole: roleAssistant, kText: "", kCalls: []map[string]any{{"id": call,
					fnKey: map[string]any{fieldName: toolTerminal, ilArgs: `{"command":"ls -la /tmp"}`}}}},
					map[string]any{kRole: stepTool, kCallID: call, fieldContent: strings.Repeat("line of output\n", 12)})
			}
			text := strings.Repeat(fmt.Sprintf("%s answer %d fills the log. ", c.id, i), 3+i%17)
			if i%9 == 4 {
				p := fmt.Sprintf("%s%s-%d.png", shots, c.id, i)
				e.gw.SetFile(p, pic.Bytes())
				text += "\n\nMEDIA:" + p
			}
			ms = append(ms, map[string]any{kRole: roleAssistant, kText: text})
		}
		e.gw.AddSession(&gatewaytest.Session{ID: c.id, Title: "Long " + c.id, Live: true, StartedAt: float64(900 - k), Messages: ms})
	}
	e.gw.SlowResume(func(id string) time.Duration { return slow[id] })
	// Two chats keep working, so the hub's copy of them is behind Hermes'
	// transcript: each turn streams a tool card and a reply, and stores it.
	go func() {
		tick := time.NewTicker(400 * time.Millisecond)
		defer tick.Stop()
		for n := 0; ; n++ {
			select {
			case <-t.Context().Done():
				return
			case <-tick.C:
			}
			id := chats[1+2*(n%2)].id
			tool := fmt.Sprintf("t%d", n)
			e.gw.SetRunning(id, true)
			e.gw.Publish(id, hub.KindToolStart, map[string]any{sjToolID: tool, fieldName: toolTerminal, ilArgs: map[string]any{}})
			e.gw.Publish(id, hub.KindToolComplete, map[string]any{sjToolID: tool, fieldName: toolTerminal, sjResult: strings.Repeat("out\n", 8)})
			e.gw.Publish(id, hub.KindMessageStart, map[string]any{})
			e.gw.Publish(id, hub.KindMessageDelta, map[string]any{kText: strings.Repeat("more words ", 40)})
			e.gw.FinishReply(id, fmt.Sprintf("%s later reply %d ", id, n)+strings.Repeat("more words ", 40))
		}
	}()
	prof, err := os.MkdirTemp("", "switchjump-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "switchjump", "run.js"), bin, e.url, prof).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
