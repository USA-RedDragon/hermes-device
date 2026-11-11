package web_test

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
)

const (
	shotDir       = "/opt/data/hermes/cache/screenshots/hermes-device/m1/"
	shotJPEG      = "\xff\xd8\xff\xe0jpeg"
	shotPNG       = "\x89PNG\r\n\x1a\npng"
	kRole         = "role"
	roleUser      = "user"
	roleAssistant = "assistant"
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
	e.gw.AddSession(&gatewaytest.Session{ID: "m1", Title: "Media", Live: true, StartedAt: 200, Messages: []map[string]any{
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
		h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("named = %d %q %v", resp.StatusCode, body, h)
	}
	if resp, body = get(t, c, mediaURL(e, "pc", tool)); resp.StatusCode != http.StatusOK || body != shotJPEG {
		t.Fatalf("tool result = %d %q", resp.StatusCode, body)
	}
	for _, p := range []string{upload, ref} {
		if resp, body = get(t, c, mediaURL(e, "pc", p)); resp.StatusCode != http.StatusOK || body != shotPNG || resp.Header.Get("Content-Type") != "image/png" {
			t.Fatalf("attached image %s = %d %q", p, resp.StatusCode, body)
		}
	}
	for _, p := range []string{report, fake} {
		resp, body = get(t, c, mediaURL(e, "pc", p))
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/octet-stream" ||
			!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("%s = %d %q %v", p, resp.StatusCode, body, resp.Header)
		}
	}
	if resp, _ = get(t, c, mediaURL(e, "pc", named)+"&download=1"); resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("download = %v", resp.Header)
	}
	before := len(e.gw.FileReads())
	for _, p := range []string{unnamed, typed, private, shotDir + "../../../../../../etc/passwd.txt", "/opt/data/hermes/cache/screenshots/hermes-device/m1/missing.jpg", "relative.jpg", ""} {
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
	e.gw.AddSession(&gatewaytest.Session{ID: "m1", Title: "Media", Live: true, StartedAt: 200})
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
