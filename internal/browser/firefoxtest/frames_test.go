package firefoxtest_test

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
)

// Hosts of the frame test servers: the page, a cross-origin frame (and a
// site the page must not reach after a script) and a protected site.
const (
	crossHost   = "127.0.0.2"
	confirmHost = "127.0.0.3"
	keyScript   = "script"
	keyWrites   = "allow_writes"
	keyFrame    = "frame"
	keyKey      = "key"
	keyTimeout  = "timeout_ms"
	keyScripts  = "script_hosts"
)

// recorder counts what the test servers got.
type recorder struct {
	mu    sync.Mutex
	puts  []string
	leaks int
}

func (r *recorder) put(body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.puts = append(r.puts, body)
}

func (r *recorder) leak() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaks++
}

func (r *recorder) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.puts), r.leaks
}

func serveOn(ctx context.Context, t *testing.T, host string, h http.Handler) *httptest.Server {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", host+":0")
	if err != nil {
		t.Fatalf("listen on %s: %v", host, err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// framePages serves the test pages: /frames has a same-origin and a
// cross-origin iframe, an open shadow root, a key field, a hover card and
// a tall body; /later adds text after a delay; /api/data answers GET and
// records PUT; /leak counts requests that must never arrive.
func framePages(cross string, rec *recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/frames":
			_, _ = io.WriteString(w, `<title>frames</title><h1>Frames</h1>`+
				`<iframe src="/inner" title="same" width="400" height="120"></iframe>`+
				`<iframe src="`+cross+`/inner" title="cross" width="400" height="120"></iframe>`+
				`<div id="host"></div><script>document.getElementById("host").attachShadow({mode: "open"}).innerHTML =`+
				` '<button onclick="this.textContent=\'clicked shadow\'">Shadow button</button><p>shadow text</p>';</script>`+
				`<input id="k" aria-label="keys"><script>document.getElementById("k").addEventListener("keydown",`+
				` (e) => { document.title = "key:" + e.key; });</script>`+
				`<button id="hv" onmouseover="document.getElementById('card').hidden = false">hover me</button>`+
				`<div id="card" hidden>card shown</div><div style="height: 5000px">tall</div><p>bottom text</p>`)
		case "/inner":
			_, _ = io.WriteString(w, `<title>inner</title><p>inner `+html.EscapeString(r.Host[:strings.LastIndex(r.Host, ":")])+`</p>`+
				`<button onclick="this.textContent='clicked inner'">Inner button</button>`)
		case "/later":
			_, _ = io.WriteString(w, `<title>later</title><p>soon</p><script>setTimeout(() =>`+
				` document.body.insertAdjacentHTML("beforeend", "<p>late text</p>"), 800);</script>`)
		case "/api/data":
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPut {
				body, _ := io.ReadAll(r.Body)
				rec.put(r.Header.Get("X-Test") + " " + string(body))
				_, _ = io.WriteString(w, `{"ok": true}`)
				return
			}
			_, _ = io.WriteString(w, `{"value": 42, "items": [1, 2]}`)
		case "/leak":
			rec.leak()
			_, _ = io.WriteString(w, "leaked")
		default:
			http.NotFound(w, r)
		}
	}
}

func mustFail(ctx context.Context, t *testing.T, b *broker.Broker, what, want, op string, args map[string]any) {
	t.Helper()
	if _, err := b.Call(ctx, op, args); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: err = %v, want %q", what, err, want)
	} else {
		t.Logf("%s refused: %v", what, err)
	}
}

// frameFlow checks frames, shadow DOM, scrolling, keys, hover, waiting,
// full-page screenshots and scripts on local test servers. It returns the
// page server's URL and its recorder for the plugin layer.
func frameFlow(ctx context.Context, t *testing.T, b *broker.Broker, tabID int) (string, *recorder) {
	t.Helper()
	rec := &recorder{}
	cross := serveOn(ctx, t, crossHost, framePages("", rec))
	page := serveOn(ctx, t, loopback, framePages(cross.URL, rec))
	confirm := serveOn(ctx, t, confirmHost, framePages("", rec))
	call(ctx, t, b, "navigate", map[string]any{keyTab: tabID, keyURL: page.URL + "/frames",
		keyGuard: map[string]any{keyVisit: []string{loopback}}}, nil)

	var snap snapResult
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID}, &snap)
	t.Logf("frames snapshot:\n%s", snap.Snapshot)
	buttons := refsByName(snap.Snapshot, `"Inner button"`)
	shadow := refByName(snap.Snapshot, `"Shadow button"`)
	if !strings.Contains(snap.Snapshot, `- iframe "same" [frame=f`) || !strings.Contains(snap.Snapshot, `- iframe "cross" [frame=f`) ||
		len(buttons) != 2 || !strings.HasPrefix(buttons[0], "f") || !strings.HasPrefix(buttons[1], "f") ||
		buttons[0][:2] == buttons[1][:2] || shadow == "" || strings.HasPrefix(shadow, "f") {
		t.Fatalf("snapshot lacks the frames or the shadow button: buttons=%q shadow=%q", buttons, shadow)
	}
	inner, crossBtn := buttons[0], buttons[1]
	scopedFlow(ctx, t, b, tabID, shadow, inner, crossBtn)
	for _, ref := range []string{inner, crossBtn, shadow} {
		call(ctx, t, b, "click", map[string]any{keyTab: tabID, keyRef: ref}, nil)
	}
	var text struct {
		Text       string `json:"text"`
		TotalChars int    `json:"total_chars"`
		NextOffset int    `json:"next_offset"`
	}
	call(ctx, t, b, "read_text", map[string]any{keyTab: tabID}, &text)
	t.Logf("frames text: %q", text.Text)
	for _, want := range []string{"inner 127.0.0.1", "inner " + crossHost, "clicked inner", "shadow text", "clicked shadow", "bottom text"} {
		if !strings.Contains(text.Text, want) {
			t.Errorf("text lacks %q", want)
		}
	}
	if strings.Count(text.Text, "clicked inner") != 2 {
		t.Errorf("both frame buttons should read clicked inner: %q", text.Text)
	}
	call(ctx, t, b, "read_text", map[string]any{keyTab: tabID, "max_chars": 10, "offset": 3}, &text)
	if len(text.Text) != 10 || text.NextOffset != 13 {
		t.Errorf("paged text = %q next %d", text.Text, text.NextOffset)
	}

	interactFlow(ctx, t, b, tabID, page.URL, snap.Snapshot)
	scriptFlow(ctx, t, b, tabID, page.URL, cross.URL, rec)
	confirmFlow(ctx, t, b, tabID, confirm.URL)
	return page.URL, rec
}

func fullPageSize(ctx context.Context, t *testing.T, b *broker.Broker, tabID int) {
	t.Helper()
	var shot struct {
		DataURL string `json:"data_url"`
	}
	call(ctx, t, b, "screenshot", map[string]any{keyTab: tabID, "full_page": true}, &shot)
	t.Logf("full page PNG data URL: %d bytes", len(shot.DataURL))
}

type evalResult struct {
	Result   json.RawMessage `json:"result"`
	ReadOnly bool            `json:"read_only"`
	Hint     string          `json:"hint"`
}

func evalIn(ctx context.Context, t *testing.T, b *broker.Broker, args map[string]any) evalResult {
	t.Helper()
	var out evalResult
	call(ctx, t, b, "eval", args, &out)
	t.Logf("eval %.60q -> %s", args[keyScript], out.Result)
	return out
}

// scriptFlow checks eval: approvals, a GET fetch, state kept on window, a
// write refused without its approval and done with it, no requests to
// other sites, and the limits on a tab after a script.
func scriptFlow(ctx context.Context, t *testing.T, b *broker.Broker, tabID int, base, cross string, rec *recorder) {
	t.Helper()
	call(ctx, t, b, "navigate", map[string]any{keyTab: tabID, keyURL: base + "/frames"}, nil)
	var snap snapResult
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID}, &snap)
	read := map[string]any{keyScripts: []string{loopback}}
	get := `const r = await fetch("/api/data"); return await r.json();`
	mustFail(ctx, t, b, "eval without approval", "needs the user's approval", "eval", map[string]any{keyTab: tabID, keyScript: get})
	if out := evalIn(ctx, t, b, map[string]any{keyTab: tabID, keyScript: get, keyGuard: read}); string(out.Result) != `{"value":42,"items":[1,2]}` || !out.ReadOnly {
		t.Errorf("GET fetch result = %s", out.Result)
	}
	evalIn(ctx, t, b, map[string]any{keyTab: tabID, keyScript: `window.__kept = {n: 7}; return true;`, keyGuard: read})
	if out := evalIn(ctx, t, b, map[string]any{keyTab: tabID, keyScript: `return window.__kept.n + document.querySelectorAll("iframe").length;`, keyGuard: read}); string(out.Result) != "9" {
		t.Errorf("state on window = %s", out.Result)
	}
	sameFrame := `const d = [...document.querySelectorAll("iframe")].map((f) => { try { return f.contentDocument; } catch (e) { return null; } }).filter(Boolean);` +
		` return d.map((x) => x.querySelector("p").textContent);`
	if out := evalIn(ctx, t, b, map[string]any{keyTab: tabID, keyScript: sameFrame, keyGuard: read}); string(out.Result) != `["inner 127.0.0.1"]` {
		t.Errorf("same-origin frame documents from a script = %s", out.Result)
	}
	if out := evalIn(ctx, t, b, map[string]any{keyTab: tabID, keyScript: `document.title`, keyGuard: read}); string(out.Result) != "null" || out.Hint == "" {
		t.Errorf("script without return: %+v", out)
	}
	mustFail(ctx, t, b, "script error", "script error: boom", "eval", map[string]any{keyTab: tabID, keyScript: `throw new Error("boom")`, keyGuard: read})

	put := `const r = await fetch("/api/data", {method: "PUT", headers: {"Content-Type": "application/json", "X-Test": "t1"}, body: JSON.stringify({row: 1})}); return r.status;`
	mustFail(ctx, t, b, "PUT in a read-only script", "NetworkError", "eval", map[string]any{keyTab: tabID, keyScript: put, keyGuard: read})
	mustFail(ctx, t, b, "write without write approval", "need the user's separate approval", "eval",
		map[string]any{keyTab: tabID, keyScript: put, keyWrites: true, keyGuard: read})
	if puts, _ := rec.counts(); puts != 0 {
		t.Fatalf("the server got %d PUTs before the write was approved", puts)
	}
	write := map[string]any{"write_hosts": []string{loopback}}
	if out := evalIn(ctx, t, b, map[string]any{keyTab: tabID, keyScript: put, keyWrites: true, keyGuard: write}); string(out.Result) != "200" || out.ReadOnly {
		t.Errorf("approved PUT = %+v", out)
	}
	if puts, _ := rec.counts(); puts != 1 || rec.puts[0] != `t1 {"row":1}` {
		t.Errorf("PUTs = %q", rec.puts)
	}

	// Nothing the script does reaches another site: the page's fetch, the
	// content script's own fetch and XMLHttpRequest, an image, a beacon
	// or a navigation.
	leak := cross + "/leak"
	for _, s := range []string{
		`try { await fetch("` + leak + `", {mode: "no-cors"}); } catch (e) { return String(e); } return "sent";`,
		`try { await globalThis.fetch("` + leak + `", {mode: "no-cors"}); } catch (e) { return String(e); } return "sent";`,
		`try { await window.fetch("` + leak + `", {mode: "no-cors"}); } catch (e) { return String(e); } return "sent";`,
		`const x = new globalThis.XMLHttpRequest(); x.open("GET", "` + leak + `"); x.send(); await new Promise((r) => setTimeout(r, 300)); return x.status;`,
		`const x = new content.XMLHttpRequest(); x.open("GET", "` + leak + `"); x.send(); await new Promise((r) => setTimeout(r, 300)); return x.status;`,
		`window.wrappedJSObject.eval('new Image().src = "` + leak + `?img"; navigator.sendBeacon("` + leak + `?b", "x")'); await new Promise((r) => setTimeout(r, 300)); return 1;`,
		`window.wrappedJSObject.eval('setTimeout(() => { location.href = "` + leak + `?nav"; }, 50)'); await new Promise((r) => setTimeout(r, 500)); return location.href;`,
	} {
		evalIn(ctx, t, b, map[string]any{keyTab: tabID, keyScript: s, keyWrites: true, keyGuard: write})
	}
	// Agent code cannot reach the extension API (storage holds the hub
	// credential), and frames still show in snapshots afterwards.
	for _, s := range []string{
		`return typeof browser + " " + typeof chrome + " " + typeof globalThis.browser + " " + typeof (function () { return this; })().browser;`,
		`try { return Object.keys(await browser.storage.local.get(null)); } catch (e) { return "no: " + e.message; }`,
	} {
		out := evalIn(ctx, t, b, map[string]any{keyTab: tabID, keyScript: s, keyWrites: true, keyGuard: write})
		if strings.Contains(string(out.Result), "object") || strings.Contains(string(out.Result), "auth") {
			t.Errorf("agent code reached the extension API: %s", out.Result)
		}
	}
	var after snapResult
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID}, &after)
	if !strings.Contains(after.Snapshot, `- iframe "same" [frame=f`) {
		t.Errorf("frames lost after a script:\n%s", after.Snapshot)
	}
	if _, leaks := rec.counts(); leaks != 0 {
		t.Errorf("%d requests reached another site from a tab that ran a script", leaks)
	}

	mustFail(ctx, t, b, "click after a script", "ran a script", "click", map[string]any{keyTab: tabID, keyRef: refByName(snap.Snapshot, `"hover me"`)})
	mustFail(ctx, t, b, "back after a script", "ran a script", "back", map[string]any{keyTab: tabID})

	// A write approval covers only its own site: a POST from the page to
	// the cross-origin frame's site (in the tab's sites after a script
	// there) stays refused.
	inCross := `return document.body.innerText;`
	mustFail(ctx, t, b, "frame script in a scripted tab", "wait until browser_navigate", "eval",
		map[string]any{keyTab: tabID, keyFrame: "f2", keyScript: inCross, keyGuard: map[string]any{keyScripts: []string{crossHost}}})

	// A script in the cross-origin frame needs that site's approval.
	call(ctx, t, b, "navigate", map[string]any{keyTab: tabID, keyURL: base + "/frames"}, nil)
	call(ctx, t, b, "wait", map[string]any{keyTab: tabID, keyText: "inner " + crossHost, keyTimeout: 5000}, nil)
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID}, &snap)
	var frame string
	for line := range strings.SplitSeq(snap.Snapshot, "\n") {
		if strings.Contains(line, `iframe "cross"`) {
			frame = strings.TrimSuffix(line[strings.LastIndex(line, "[frame=")+len("[frame="):], "]")
		}
	}
	inFrame := map[string]any{keyTab: tabID, keyFrame: frame, keyScript: `return document.body.innerText;`, keyGuard: read}
	mustFail(ctx, t, b, "script in a cross-origin frame with the page's approval", "needs the user's approval", "eval", inFrame)
	inFrame[keyGuard] = map[string]any{keyScripts: []string{crossHost}}
	if out := evalIn(ctx, t, b, inFrame); !strings.Contains(string(out.Result), "inner "+crossHost) {
		t.Errorf("cross-origin frame text = %s", out.Result)
	}
	post := `try { await fetch("` + cross + `/leak", {method: "POST", mode: "no-cors", body: "x"}); } catch (e) { return String(e); } return "sent";`
	evalIn(ctx, t, b, map[string]any{keyTab: tabID, keyScript: post, keyWrites: true, keyGuard: write})
	if _, leaks := rec.counts(); leaks != 0 {
		t.Error("a write approved for the page reached the frame's site")
	}

	// Loading a page again ends the limits.
	call(ctx, t, b, "navigate", map[string]any{keyTab: tabID, keyURL: base + "/frames"}, nil)
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID}, &snap)
	call(ctx, t, b, "click", map[string]any{keyTab: tabID, keyRef: refByName(snap.Snapshot, `"hover me"`)}, nil)
}

// confirmFlow checks a protected site (browser.confirm-hosts, as for
// Salesforce): clicks need the user's OK each time and scripts may only
// read.
func confirmFlow(ctx context.Context, t *testing.T, b *broker.Broker, tabID int, base string) {
	t.Helper()
	call(ctx, t, b, "navigate", map[string]any{keyTab: tabID, keyURL: base + "/frames",
		keyGuard: map[string]any{keyVisit: []string{confirmHost}}}, nil)
	var info struct {
		Protected bool `json:"protected"`
	}
	call(ctx, t, b, "inspect", map[string]any{keyTab: tabID}, &info)
	if !info.Protected {
		t.Error("inspect does not report the protected site")
	}
	var snap snapResult
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID}, &snap)
	button := refByName(snap.Snapshot, `"hover me"`)
	mustFail(ctx, t, b, "click on a protected site", "needs the user's approval", "click", map[string]any{keyTab: tabID, keyRef: button})
	mustFail(ctx, t, b, "Enter on a protected site", "needs the user's approval", "press", map[string]any{keyTab: tabID, keyRef: button, keyKey: "Enter"})
	call(ctx, t, b, "press", map[string]any{keyTab: tabID, keyRef: button, keyKey: "Tab"}, nil)
	call(ctx, t, b, "click", map[string]any{keyTab: tabID, keyRef: button, keyGuard: map[string]any{"confirmed": true}}, nil)
	mustFail(ctx, t, b, "write script on a protected site", "never allowed", "eval", map[string]any{keyTab: tabID,
		keyScript: `return 1`, keyWrites: true, keyGuard: map[string]any{"write_hosts": []string{confirmHost}}})
	out := evalIn(ctx, t, b, map[string]any{keyTab: tabID, keyScript: `return document.querySelectorAll("iframe").length;`,
		keyGuard: map[string]any{keyScripts: []string{confirmHost}}})
	if string(out.Result) != "2" {
		t.Errorf("read-only script on a protected site = %s", out.Result)
	}
}

// scopedFlow checks snapshots limited by a selector or a ref: they reach
// into shadow roots and frames, and keep the refs of the full snapshot.
func scopedFlow(ctx context.Context, t *testing.T, b *broker.Broker, tabID int, shadow, inner, crossBtn string) {
	t.Helper()
	var snap snapResult
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID, keySelector: "#host"}, &snap)
	t.Logf("selector #host:\n%s", snap.Snapshot)
	if refByName(snap.Snapshot, `"Shadow button"`) != shadow || strings.Contains(snap.Snapshot, "Inner button") ||
		strings.Contains(snap.Snapshot, "hover me") {
		t.Errorf("selector #host should outline only the shadow tree, with its ref %s", shadow)
	}
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID, keyRef: crossBtn}, &snap)
	t.Logf("ref %s:\n%s", crossBtn, snap.Snapshot)
	if got := refsByName(snap.Snapshot, `"Inner button"`); len(got) != 1 || got[0] != crossBtn {
		t.Errorf("ref %s should outline that button only, got %q", crossBtn, got)
	}
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID, keySelector: "button"}, &snap)
	t.Logf("selector button:\n%s", snap.Snapshot)
	if got := refsByName(snap.Snapshot, `"Inner button"`); len(got) != 2 || got[0] != inner || got[1] != crossBtn ||
		refByName(snap.Snapshot, `"Shadow button"`) != shadow || strings.Contains(snap.Snapshot, "Frames") {
		t.Errorf("selector button should list the buttons of the page and both frames with their refs, got %q", got)
	}
	mustFail(ctx, t, b, "bad selector", "not a valid CSS selector", "snapshot", map[string]any{keyTab: tabID, keySelector: "[["})
	mustFail(ctx, t, b, "stale ref", "unknown or stale ref", "snapshot", map[string]any{keyTab: tabID, keyRef: "e999"})
}

// refsByName returns the refs of every snapshot line that contains name.
func refsByName(snapshot, name string) []string {
	var out []string
	for line := range strings.SplitSeq(snapshot, "\n") {
		if i := strings.LastIndex(line, "[ref="); i >= 0 && strings.Contains(line, name) {
			out = append(out, strings.TrimSuffix(line[i+len("[ref="):], "]"))
		}
	}
	return out
}

// interactFlow checks scrolling, keys, hover, a full-page screenshot and
// waiting on the frames page.
func interactFlow(ctx context.Context, t *testing.T, b *broker.Broker, tabID int, base, snapshot string) {
	t.Helper()
	snap := snapResult{Snapshot: snapshot}
	var text struct {
		Text string `json:"text"`
	}
	var pos struct {
		ScrollY  int  `json:"scroll_y"`
		AtBottom bool `json:"at_bottom"`
		Found    bool `json:"found"`
	}
	call(ctx, t, b, "scroll", map[string]any{keyTab: tabID, "direction": "down"}, &pos)
	if pos.ScrollY <= 0 || pos.AtBottom {
		t.Errorf("scroll down: %+v", pos)
	}
	call(ctx, t, b, "scroll", map[string]any{keyTab: tabID, "direction": "bottom"}, &pos)
	if !pos.AtBottom {
		t.Errorf("scroll to the bottom: %+v", pos)
	}
	call(ctx, t, b, "scroll", map[string]any{keyTab: tabID, "to_text": "Frames"}, &pos)
	if !pos.Found || pos.ScrollY != 0 {
		t.Errorf("scroll to the heading: %+v", pos)
	}
	call(ctx, t, b, "scroll", map[string]any{keyTab: tabID, "to_text": "no such words"}, &pos)
	if pos.Found {
		t.Error("scroll found text that is not there")
	}

	keys := refByName(snap.Snapshot, `"keys"`)
	var pressed struct {
		Title         string `json:"title"`
		DefaultAction string `json:"default_action"`
		Focused       string `json:"focused"`
	}
	call(ctx, t, b, "press", map[string]any{keyTab: tabID, keyRef: keys, keyKey: "x"}, &pressed)
	if pressed.Title != "key:x" || pressed.DefaultAction != "typed" {
		t.Errorf("press x: %+v", pressed)
	}
	call(ctx, t, b, "press", map[string]any{keyTab: tabID, keyKey: "Tab"}, &pressed)
	if pressed.DefaultAction != "focus moved" || !strings.HasPrefix(pressed.Focused, "button") {
		t.Errorf("press Tab: %+v", pressed)
	}
	call(ctx, t, b, "press", map[string]any{keyTab: tabID, keyKey: "Escape"}, &pressed)
	mustFail(ctx, t, b, "unknown key", "unknown key", "press", map[string]any{keyTab: tabID, keyKey: "F13"})
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID}, &snap)
	if !strings.Contains(snap.Snapshot, `value="x"`) {
		t.Errorf("the key field lacks the typed x:\n%s", snap.Snapshot)
	}
	call(ctx, t, b, "hover", map[string]any{keyTab: tabID, keyRef: refByName(snap.Snapshot, `"hover me"`)}, nil)
	call(ctx, t, b, "read_text", map[string]any{keyTab: tabID}, &text)
	if !strings.Contains(text.Text, "card shown") {
		t.Error("hover did not open the card")
	}

	var shot struct {
		PageHeight int  `json:"page_height"`
		Clipped    bool `json:"clipped"`
	}
	call(ctx, t, b, "screenshot", map[string]any{keyTab: tabID, "full_page": true}, &shot)
	if shot.PageHeight < 5000 || shot.Clipped {
		t.Errorf("full page screenshot: %+v", shot)
	}
	fullPageSize(ctx, t, b, tabID)

	var waited struct {
		Found bool `json:"found"`
	}
	call(ctx, t, b, "navigate", map[string]any{keyTab: tabID, keyURL: base + "/later"}, nil)
	call(ctx, t, b, "wait", map[string]any{keyTab: tabID, keyText: "late text", keyTimeout: 5000}, &waited)
	if !waited.Found {
		t.Error("wait did not see the late text")
	}
	call(ctx, t, b, "wait", map[string]any{keyTab: tabID, keySelector: "#nothing", keyTimeout: 500}, &waited)
	if waited.Found {
		t.Error("wait found a selector that matches nothing")
	}
	mustFail(ctx, t, b, "bad selector", "bad CSS selector", "wait", map[string]any{keyTab: tabID, keySelector: "[[", keyTimeout: 100})
}
