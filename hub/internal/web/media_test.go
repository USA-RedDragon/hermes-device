package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

const (
	shotDir       = "/opt/data/hermes/cache/screenshots/hermes-device/m1/"
	shotJPEG      = "\xff\xd8\xff\xe0jpeg"
	shotPNG       = "\x89PNG\r\n\x1a\npng"
	kRole         = "role"
	roleUser      = "user"
	roleAssistant = "assistant"
	titleMedia    = "Media"
	octetStream   = "application/octet-stream"
	nosniff       = "nosniff"
	mimePNG       = "image/png"
)

func mediaURL(e *hubEnv, backend, p string) string {
	return e.url + "/api/sessions/" + backend + "/m1/media?path=" + url.QueryEscape(p)
}

func TestMedia(t *testing.T) {
	t.Parallel()
	e := setup(t)
	named, tool, report := shotDir+"named.jpg", shotDir+"tool.jpg", shotDir+"report.pdf"
	fake, unnamed, private := shotDir+"fake.png", shotDir+"unnamed.jpg", "/opt/data/hermes/config.yaml"
	upload, ref, typed := "/opt/data/hermes/cache/images/up.png", "/opt/data/hermes/cache/images/my ref.png", "/opt/data/hermes/cache/images/typed.png"
	for p, data := range map[string]string{named: shotJPEG, tool: shotJPEG, report: "%PDF-1.7", fake: "<script>alert(1)</script>",
		unnamed: shotJPEG, private: "key: value", upload: shotPNG, ref: shotPNG, typed: shotPNG} {
		e.gw.SetFile(p, []byte(data))
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "m1", Title: titleMedia, Live: true, StartedAt: 200, Messages: []map[string]any{
		{kRole: roleUser, kText: "what is this?\n\n[Image attached at: " + upload + "]\n@image:`" + ref + "`"},
		{kRole: roleUser, kText: "MEDIA:" + typed + " and see [Image attached at: " + typed + "]"},
		{kRole: roleUser, kText: "screenshot please"},
		{kRole: roleAssistant, kText: "", "tool_calls": []map[string]any{{"id": "c1", "function": map[string]any{"name": "browser_screenshot"}}}},
		{kRole: "tool", "tool_call_id": "c1", "content": `{"path":"` + tool + `","show_user":"MEDIA:` + tool + `"}`},
		{kRole: roleAssistant, kText: "Here:\nMEDIA:" + named + "\nMEDIA:" + report + "\nMEDIA:" + fake + "\nMEDIA:" + private},
		{kRole: roleUser, kText: "MEDIA:" + unnamed},
	}})
	c := client(t)
	login(t, e, c)

	resp, body := get(t, c, mediaURL(e, "pc", named))
	h := resp.Header
	if resp.StatusCode != http.StatusOK || body != shotJPEG || h.Get("Content-Type") != "image/jpeg" ||
		!strings.HasPrefix(h.Get("Content-Disposition"), "inline") || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") ||
		h.Get("X-Content-Type-Options") != nosniff {
		t.Fatalf("named = %d %q %v", resp.StatusCode, body, h)
	}
	if resp, body = get(t, c, mediaURL(e, "pc", tool)); resp.StatusCode != http.StatusOK || body != shotJPEG {
		t.Fatalf("tool result = %d %q", resp.StatusCode, body)
	}
	for _, p := range []string{upload, ref} {
		if resp, body = get(t, c, mediaURL(e, "pc", p)); resp.StatusCode != http.StatusOK || body != shotPNG || resp.Header.Get("Content-Type") != mimePNG {
			t.Fatalf("attached image %s = %d %q", p, resp.StatusCode, body)
		}
	}
	for _, p := range []string{fake} {
		resp, body = get(t, c, mediaURL(e, "pc", p))
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != octetStream ||
			!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("%s = %d %q %v", p, resp.StatusCode, body, resp.Header)
		}
	}
	if resp, _ = get(t, c, mediaURL(e, "pc", named)+"&download=1"); resp.Header.Get("Content-Type") != octetStream {
		t.Fatalf("download = %v", resp.Header)
	}
	before := len(e.gw.FileReads())
	for _, p := range []string{report, unnamed, typed, private, shotDir + "../../../../../../etc/passwd.txt", "/opt/data/hermes/cache/screenshots/hermes-device/m1/missing.jpg", "relative.jpg", ""} {
		if resp, body = get(t, c, mediaURL(e, "pc", p)); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%q = %d %s", p, resp.StatusCode, body)
		}
	}
	if resp, _ = get(t, c, mediaURL(e, backendHome, named)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other backend = %d", resp.StatusCode)
	}
	if reads := e.gw.FileReads(); len(reads) != before {
		t.Fatalf("refused paths were read: %v", reads[before:])
	}

	anon := client(t)
	if resp, _ = get(t, anon, mediaURL(e, "pc", named)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", resp.StatusCode)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, mediaURL(e, "pc", named), nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	cross, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = cross.Body.Close()
	if cross.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site = %d", cross.StatusCode)
	}
}

func TestMediaFromLiveReply(t *testing.T) {
	t.Parallel()
	e := setup(t)
	later := shotDir + "later.jpg"
	e.gw.SetFile(later, []byte(shotJPEG))
	e.gw.AddSession(&gatewaytest.Session{ID: "m1", Title: titleMedia, Live: true, StartedAt: 200})
	c := client(t)
	login(t, e, c)
	if resp, _ := get(t, c, mediaURL(e, "pc", later)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("before the reply = %d", resp.StatusCode)
	}
	e.gw.Publish("m1", "message.start", map[string]any{})
	e.gw.Publish("m1", "message.delta", map[string]any{kText: "Here it is.\nMEDIA:" + shotDir})
	e.gw.Publish("m1", "message.delta", map[string]any{kText: "later.jpg"})
	e.gw.Publish("m1", "message.complete", map[string]any{"status": "complete"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, body := get(t, c, mediaURL(e, "pc", later))
		if resp.StatusCode == http.StatusOK && body == shotJPEG {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the reply = %d %s", resp.StatusCode, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reads := e.gw.FileReads(); !slices.Contains(reads, later) {
		t.Fatalf("reads = %v", reads)
	}
}

func TestMediaRootsServeFiles(t *testing.T) {
	t.Parallel()
	e := setupWith(t, func(cfg *config.Config, _ *web.Options, _ *store.Store) {
		cfg.Backends[0].MediaRoots = []string{"/srv/out"}
	})
	report, shot := "/srv/out/report.pdf", shotDir+"named.jpg"
	e.gw.SetFile(report, []byte("%PDF-1.7"))
	e.gw.SetFile(shot, []byte(shotJPEG))
	e.gw.AddSession(&gatewaytest.Session{ID: "m1", Title: titleMedia, Live: true, StartedAt: 200, Messages: []map[string]any{
		{kRole: roleAssistant, kText: "MEDIA:" + report + "\nMEDIA:" + shot}}})
	c := client(t)
	login(t, e, c)
	resp, body := get(t, c, mediaURL(e, "pc", report))
	if resp.StatusCode != http.StatusOK || body != "%PDF-1.7" || resp.Header.Get("Content-Type") != octetStream ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("report = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if resp, _ = get(t, c, mediaURL(e, "pc", shot)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outside the roots = %d", resp.StatusCode)
	}
}

// deviceFiles is a device that runs every chat, with files in memory.
type deviceFiles struct {
	mu      sync.Mutex
	files   map[string]string
	offline bool
	reads   []string
}

func (d *deviceFiles) PutFile(context.Context, string, string, string, string, []byte) (string, bool, error) {
	return "", false, nil
}

func (d *deviceFiles) DeviceMedia(_ context.Context, user, backendID, sessionID, p string) (*hub.MediaFile, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads = append(d.reads, user+"|"+backendID+"|"+sessionID+"|"+p)
	if d.offline {
		return nil, true, hub.ErrMediaOffline
	}
	data, ok := d.files[p]
	if !ok {
		return nil, true, hub.ErrNoMedia
	}
	return &hub.MediaFile{Name: path.Base(p), Size: int64(len(data)), NoStore: true, Body: io.NopCloser(strings.NewReader(data))}, true, nil
}

func (d *deviceFiles) read() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.reads)
}

func TestMediaFromTheDevice(t *testing.T) {
	t.Parallel()
	e := setup(t)
	chart, notes, fake, unnamed := "/home/alice/src/chart.png", "/home/alice/src/notes.txt", "/home/alice/src/fake.png", "/home/alice/src/secret.txt"
	shot, onDevice, report := shotDir+"server.jpg", shotDir+"device.jpg", "/home/alice/src/report.html"
	dev := &deviceFiles{files: map[string]string{chart: shotPNG, notes: textHello, fake: "<svg onload=alert(1)>", unnamed: "no",
		shot: "device copy", onDevice: shotJPEG, report: "<!doctype html><title>Report</title>"}}
	e.svc.SetFiles(dev)
	e.gw.SetFile(shot, []byte(shotJPEG))
	e.gw.AddSession(&gatewaytest.Session{ID: "m1", Title: titleMedia, Live: true, StartedAt: 200, Messages: []map[string]any{
		{kRole: roleAssistant, kText: "MEDIA:" + chart + "\nMEDIA:" + notes + "\nMEDIA:" + fake + "\nMEDIA:" + shot + "\nMEDIA:" + onDevice + "\nMEDIA:" + report},
	}})
	c := client(t)
	login(t, e, c)
	resp, body := get(t, c, mediaURL(e, "pc", chart))
	if resp.StatusCode != http.StatusOK || body != shotPNG || resp.Header.Get("Content-Type") != hub.MIMEPNG ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") ||
		resp.Header.Get("Content-Security-Policy") != "default-src 'none'; sandbox" || resp.Header.Get("X-Content-Type-Options") != nosniff ||
		resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("chart = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	for _, p := range []string{notes, fake} {
		resp, body = get(t, c, mediaURL(e, "pc", p))
		if resp.StatusCode != http.StatusOK || body != dev.files[p] || resp.Header.Get("Content-Type") != octetStream ||
			!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("%s = %d %q %v", p, resp.StatusCode, body, resp.Header)
		}
	}
	if resp, body = get(t, c, mediaURL(e, "pc", shot)); resp.StatusCode != http.StatusOK || body != shotJPEG ||
		resp.Header.Get("Cache-Control") != "private, max-age=300" {
		t.Fatalf("a server file = %d %q", resp.StatusCode, body)
	}
	if resp, body = get(t, c, mediaURL(e, "pc", onDevice)); resp.StatusCode != http.StatusOK || body != shotJPEG {
		t.Fatalf("a media root path only on the device = %d %q", resp.StatusCode, body)
	}
	pageInfo := e.url + "/api/sessions/pc/m1/page-info?path=" + url.QueryEscape(report)
	if resp, body = get(t, c, pageInfo); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"Report"`) {
		t.Fatalf("a device page = %d %s", resp.StatusCode, body)
	}
	before := len(dev.read())
	if resp, _ = get(t, c, mediaURL(e, "pc", unnamed)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unnamed = %d", resp.StatusCode)
	}
	if resp, _ = get(t, c, mediaURL(e, backendHome, chart)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other backend = %d", resp.StatusCode)
	}
	if reads := dev.read(); len(reads) != before {
		t.Fatalf("refused paths were read: %v", reads[before:])
	}
	for _, r := range dev.read() {
		if !strings.HasPrefix(r, userAlice+"|pc|m1|") {
			t.Fatalf("read = %q", r)
		}
	}
	dev.mu.Lock()
	dev.offline = true
	dev.mu.Unlock()
	if resp, body = get(t, c, mediaURL(e, "pc", chart)); resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "offline") {
		t.Fatalf("offline = %d %s", resp.StatusCode, body)
	}
	if resp, _ = get(t, c, pageInfo); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an offline page = %d", resp.StatusCode)
	}
}

func TestMediaFromDeviceNote(t *testing.T) {
	t.Parallel()
	e := setup(t)
	pasted, notes, other := "/home/alice/src/.hermes/attachments/image.png", "/home/alice/src/.hermes/attachments/notes.txt", "/home/alice/src/other.png"
	dev := &deviceFiles{files: map[string]string{pasted: shotPNG, notes: textHello, other: shotPNG}}
	e.svc.SetFiles(dev)
	e.gw.AddSession(&gatewaytest.Session{ID: "n1", Title: titleMedia, Live: true, StartedAt: 200, Messages: []map[string]any{
		{kRole: roleUser, kText: hub.FileNote("image.png", pasted) + "\n" + hub.FileNote("notes.txt", notes)},
	}})
	c := client(t)
	login(t, e, c)
	getNote := func(p string) (*http.Response, string) {
		return get(t, c, e.url+"/api/sessions/pc/n1/media?path="+url.QueryEscape(p))
	}
	if resp, body := getNote(pasted); resp.StatusCode != http.StatusOK || body != shotPNG || resp.Header.Get("Content-Type") != hub.MIMEPNG {
		t.Fatalf("an image the note names = %d %q %v", resp.StatusCode, body, resp.Header)
	}
	for _, p := range []string{notes, other} {
		if resp, _ := getNote(p); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s = %d, want not found", p, resp.StatusCode)
		}
	}
}

func TestMediaNamedByMarkdown(t *testing.T) {
	t.Parallel()
	e := setup(t)
	img, link, spaced := shotDir+"img.png", shotDir+"link.png", shotDir+"my pic.png"
	unnamed, inCode, remote := shotDir+"unnamed.png", shotDir+"code.png", shotDir+"remote.png"
	for _, p := range []string{img, link, spaced, unnamed, inCode, remote} {
		e.gw.SetFile(p, []byte(shotPNG))
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "m1", Title: titleMedia, Live: true, StartedAt: 200, Messages: []map[string]any{
		{kRole: roleUser, kText: "![user](" + unnamed + ")"},
		{kRole: roleAssistant, kText: "Here: ![a plot](" + img + ")\nAnd [the file](" + link + ") and ![two words](<" + spaced + ">)\n" +
			"Not these: `![x](" + inCode + ")` ![rel](shot.png) [far](https://example.com" + remote + ")"},
	}})
	c := client(t)
	login(t, e, c)
	for _, p := range []string{img, link, spaced} {
		if resp, body := get(t, c, mediaURL(e, "pc", p)); resp.StatusCode != http.StatusOK || body != shotPNG || resp.Header.Get("Content-Type") != mimePNG {
			t.Fatalf("%s = %d %q", p, resp.StatusCode, body)
		}
	}
	for _, p := range []string{unnamed, inCode, remote, "shot.png"} {
		if resp, body := get(t, c, mediaURL(e, "pc", p)); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%q = %d %s", p, resp.StatusCode, body)
		}
	}
}

// The image a vision_analyze or browser_vision call looks at is served to
// the session that made the call, as the call's card shows it, and to no
// other session. A non-image path and a bare name stay refused.
func TestMediaNamedByVisionCall(t *testing.T) {
	t.Parallel()
	e := setup(t)
	seen, page, batched, other := shotDir+"seen.png", shotDir+"page.jpg", shotDir+"batched.png", shotDir+"other.png"
	notImage, unnamed := "/opt/data/hermes/config.yaml", shotDir+"unnamed.png"
	for _, p := range []string{seen, page, batched, other, unnamed} {
		e.gw.SetFile(p, []byte(shotPNG))
	}
	e.gw.SetFile(notImage, []byte("key: value"))
	call := func(id, name, args string) map[string]any {
		return map[string]any{kRole: roleAssistant, kText: "", "tool_calls": []map[string]any{{"id": id, "function": map[string]any{"name": name, "arguments": args}}}}
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "m1", Title: titleMedia, Live: true, StartedAt: 200, Messages: []map[string]any{
		call("c1", "vision_analyze", `{"image_url":"`+seen+`","question":"what is it?"}`),
		call("c2", "browser_vision", `{"image_url":"`+page+`"}`),
		call("c3", "tool_call", `{"calls":[{"name":"vision_analyze","arguments":{"image_url":"`+batched+`"}}]}`),
		call("c4", "vision_analyze", `{"image_url":"`+notImage+`"}`),
		call("c5", "terminal", `{"command":"cat `+unnamed+`","image_url":"`+unnamed+`"}`),
	}})
	e.gw.AddSession(&gatewaytest.Session{ID: "m2", Title: "Other", Live: true, StartedAt: 100, Messages: []map[string]any{
		call("d1", "vision_analyze", `{"image_url":"`+other+`"}`),
	}})
	c := client(t)
	login(t, e, c)
	for _, p := range []string{seen, page, batched} {
		resp, body := get(t, c, mediaURL(e, "pc", p))
		if resp.StatusCode != http.StatusOK || body != shotPNG || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") {
			t.Fatalf("%s = %d %q %v", p, resp.StatusCode, body, resp.Header)
		}
	}
	for _, p := range []string{other, notImage, unnamed} {
		if resp, body := get(t, c, mediaURL(e, "pc", p)); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s from the first session = %d %s", p, resp.StatusCode, body)
		}
	}
	if resp, body := get(t, c, e.url+"/api/sessions/pc/m2/media?path="+url.QueryEscape(seen)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("another session's image = %d %s", resp.StatusCode, body)
	}
	if resp, body := get(t, c, e.url+"/api/sessions/pc/m2/media?path="+url.QueryEscape(other)); resp.StatusCode != http.StatusOK || body != shotPNG {
		t.Fatalf("its own image = %d %q", resp.StatusCode, body)
	}
}
