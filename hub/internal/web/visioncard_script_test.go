package web_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// shotPicture is a PNG that looks like a screenshot: a dark window with a
// title bar and a few lines of text, so a thumbnail and a full screen view
// differ from a blank square.
func shotPicture(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 640, 400))
	for y := range 400 {
		for x := range 640 {
			c := color.RGBA{18, 22, 30, 255}
			switch {
			case y < 28:
				c = color.RGBA{47, 111, 222, 255}
			case y > 60 && y < 70 && x > 24 && x < 420, y > 90 && y < 100 && x > 24 && x < 300, y > 120 && y < 130 && x > 24 && x < 360:
				c = color.RGBA{200, 205, 212, 255}
			case y > 300 && y < 340 && x > 440 && x < 600:
				c = color.RGBA{31, 157, 85, 255}
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// The card of a vision_analyze call shows the image: a thumbnail beside the
// question that opens full screen, the image and its path when open, a
// placeholder and never a broken image for a file the hub cannot serve, and
// only a link for a web address. testdata/visioncard/run.js drives the real
// page in headless Chrome, touch and desktop, light and dark. VISIONCARD_SHOT=
// prefix also writes screenshots. It skips where node or Chrome is missing.
func TestVisionCardShowsTheImage(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	const (
		seen  = "/opt/data/hermes/cache/images/s1.png"
		gone  = "/opt/data/hermes/cache/images/gone.png"
		other = "/home/reddragon/.cache/relift/win-tests/s1.png"
	)
	e.gw.SetCapabilities(gateway.CapTranscriptToolDetails)
	e.gw.SetFile(seen, shotPicture(t))
	call := func(id, args string) map[string]any {
		return map[string]any{kRole: roleAssistant, kText: "Looking.", kCalls: []any{map[string]any{"id": id, fnKey: map[string]any{fieldName: "vision_analyze", ilArgs: args}}}}
	}
	result := func(id string) map[string]any {
		return map[string]any{kRole: stepTool, kCallID: id, fieldName: "vision_analyze", kText: "Image loaded into your context \u2014 you can see it natively now. Use your built-in vision to answer the user.\n\nQuestion: q",
			kTimestamp: 1763000000.0, "display_metadata": map[string]any{"tool_outcome": map[string]any{"duration_ms": 309}}}
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "s9", Title: "Vision", Live: true, StartedAt: 900, Messages: []map[string]any{
		{kRole: roleUser, kText: "what is on screen?"},
		call("c1", `{"image_url":"`+seen+`","question":"What is on screen? Describe the window title, any error text and which button looks focused."}`), result("c1"),
		call("c2", `{"image_url":"`+gone+`","question":"What is on screen?"}`), result("c2"),
		call("c3", `{"image_url":"https://example.com/charts/q3.png","question":"Which chart is this?"}`), result("c3"),
		call("c4", `{"image_url":"`+other+`","question":"Is this device-only image shown?"}`), result("c4"),
		{kRole: roleAssistant, kText: "done"},
	}})
	prof, err := os.MkdirTemp("", "visioncard-profile-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(prof) })
	out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "visioncard", "run.js"), chrome, e.url, prof, seen, gone).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
