// Package firefoxtest_test drives a real, throwaway, headless Firefox
// through the Hermes extension and the native host. It runs only with
// HERMES_BROWSER_E2E=1 and never touches the user's own Firefox profile:
// HOME, the XDG directories and the profile all live in a fresh temporary
// directory, and Firefox runs with --no-remote in its own process group.
package firefoxtest_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/extension/firefox"
	"github.com/USA-RedDragon/hermes-device/internal/browser/bidi"
	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
	"github.com/USA-RedDragon/hermes-device/internal/browser/host"
)

const userJS = `user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("datareporting.policy.dataSubmissionEnabled", false);
user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);
user_pref("browser.aboutwelcome.enabled", false);
`

const (
	keyTab         = "tab_id"
	keyURL         = "url"
	keyRef         = "ref"
	keyGuard       = "guard"
	keyText        = "text"
	keyInteractive = "interactive_only"
	loopback       = "127.0.0.1"
	keyVisit       = "visit_hosts"
	exampleHost    = "example.com"
	exampleURL     = "https://example.com/"
)

type tabResult struct {
	TabID   int    `json:"tab_id"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	GroupID int    `json:"group_id"`
}

type textResult struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Text  string `json:"text"`
}

type snapResult struct {
	Snapshot string `json:"snapshot"`
}

type inspectResult struct {
	Host      string   `json:"host"`
	Sensitive []string `json:"sensitive"`
	Target    struct {
		Kind string `json:"kind"`
		Host string `json:"host"`
	} `json:"target"`
}

func TestFirefoxEndToEnd(t *testing.T) { //nolint:paralleltest // one Firefox at a time
	if os.Getenv("HERMES_BROWSER_E2E") != "1" {
		t.Skip("set HERMES_BROWSER_E2E=1 to drive a throwaway headless Firefox")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	b, bc := startFirefox(ctx, t)

	var tab tabResult
	if _, err := b.Call(ctx, "tab_open", map[string]any{keyURL: exampleURL}); err == nil ||
		!strings.Contains(err.Error(), "needs the user's approval") {
		t.Errorf("tab_open without approval: err = %v", err)
	}
	call(ctx, t, b, "tab_open", map[string]any{keyURL: exampleURL,
		keyGuard: map[string]any{keyVisit: []string{exampleHost}}}, &tab)
	t.Logf("tab_open: id=%d url=%s title=%q group=%d", tab.TabID, tab.URL, tab.Title, tab.GroupID)
	if tab.Title != "Example Domain" {
		t.Errorf("title = %q", tab.Title)
	}
	t.Logf("navigator.webdriver under --remote-debugging-port: %s", evalInTab(ctx, bc, exampleHost, "navigator.webdriver"))

	var text textResult
	call(ctx, t, b, "read_text", map[string]any{keyTab: tab.TabID}, &text)
	t.Logf("read_text: title=%q text=%.160q", text.Title, text.Text)
	if !strings.Contains(text.Text, "This domain is for use in") {
		t.Errorf("page text = %q", text.Text)
	}

	var snap snapResult
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tab.TabID}, &snap)
	t.Logf("snapshot:\n%s", snap.Snapshot)
	if !strings.Contains(snap.Snapshot, `- paragraph "This domain is for use in`) ||
		!strings.Contains(snap.Snapshot, `- link "Learn more" [ref=`) {
		t.Errorf("snapshot lacks the paragraph or the link: %s", snap.Snapshot)
	}

	screenshot(ctx, t, b, tab.TabID)

	ref := refs(snap.Snapshot)["link"]
	if ref == "" {
		t.Fatal("no link in the snapshot")
	}
	var info inspectResult
	call(ctx, t, b, "inspect", map[string]any{keyTab: tab.TabID, keyRef: ref}, &info)
	t.Logf("inspect %s: %+v", ref, info)
	if info.Host != exampleHost || info.Target.Kind != "link" || info.Target.Host == exampleHost {
		t.Errorf("inspect of the link = %+v", info)
	}
	if _, err := b.Call(ctx, "click", map[string]any{keyTab: tab.TabID, keyRef: ref}); err == nil ||
		!strings.Contains(err.Error(), "needs the user's approval") {
		t.Errorf("click on a link to another site without approval: err = %v", err)
	}
	var clicked tabResult
	call(ctx, t, b, "click", map[string]any{keyTab: tab.TabID, keyRef: "@" + ref,
		keyGuard: map[string]any{keyVisit: []string{info.Target.Host}}}, &clicked)
	t.Logf("click %s: now at %s", ref, clicked.URL)
	if clicked.URL == exampleURL {
		t.Error("clicking the link did not navigate")
	}

	srv := httptest.NewServer(http.HandlerFunc(formPage))
	defer srv.Close()
	formFlow(ctx, t, b, tab.TabID, srv.URL)
	refusals(ctx, t, b, tab.TabID)
	t.Run("plugin", func(t *testing.T) { pluginLayer(ctx, t, b, srv.URL) })
}

// startFirefox builds hermes-device, registers it as the native host in a
// fresh HOME, starts a headless Firefox on a fresh profile, loads the
// extension as a temporary add-on over BiDi and waits for it to connect.
func startFirefox(ctx context.Context, t *testing.T) (*broker.Broker, *bidi.Client) {
	t.Helper()
	root, err := os.MkdirTemp("", "hbe2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	home := filepath.Join(root, "home")
	profile := filepath.Join(root, "profile")
	extDir := filepath.Join(root, "ext")
	for _, d := range []string{home, profile, extDir, host.ManifestDir(home)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(root, "hermes-device")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, "../../..").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	manifest, err := json.Marshal(host.NewManifest(bin))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(host.ManifestDir(home), host.Name+".json"), manifest)
	writeFile(t, filepath.Join(profile, "user.js"), []byte(userJS))
	if err := os.CopyFS(extDir, firefox.Files); err != nil {
		t.Fatal(err)
	}

	sock := filepath.Join(root, "state", broker.SocketName)
	b, err := broker.Listen(broker.Options{Path: sock, ExtensionID: host.ExtensionID, AllowHosts: []string{loopback}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = b.Serve(ctx) }()

	ffDone := runFirefox(ctx, t, profile, []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
		"TMPDIR=" + os.TempDir(),
		"PATH=" + os.Getenv("PATH"),
		"MOZ_HEADLESS=1",
		broker.SocketEnv + "=" + sock,
	})

	port := waitBiDiPort(ctx, t, profile, ffDone)
	bc, err := bidi.Dial(ctx, "127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bc.Close(context.WithoutCancel(ctx)) })
	if err := bc.NewSession(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := bc.InstallTemporary(ctx, extDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("installed temporary extension %s", id)

	wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	hello, err := b.WaitConnected(wctx)
	if err != nil {
		t.Fatalf("extension did not connect through the native host: %v", err)
	}
	t.Logf("extension connected: %+v", hello)
	return b, bc
}

// runFirefox starts Firefox in its own process group and stops that group
// when the test ends.
func runFirefox(ctx context.Context, t *testing.T, profile string, env []string) <-chan error {
	t.Helper()
	bin := os.Getenv("HERMES_BROWSER_FIREFOX")
	if bin == "" {
		bin = "/usr/bin/firefox"
	}
	ff := exec.CommandContext(ctx, bin, "--headless", "--no-remote", "--profile", profile, //nolint:gosec // test-controlled binary
		"--remote-debugging-port=0", "about:blank")
	ff.Env = env
	var log bytes.Buffer
	ff.Stdout, ff.Stderr = &log, &log
	ff.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	ff.Cancel = func() error { return syscall.Kill(-ff.Process.Pid, syscall.SIGKILL) }
	if err := ff.Start(); err != nil {
		t.Fatal(err)
	}
	t.Logf("firefox pid %d (own process group), profile %s", ff.Process.Pid, profile)
	done := make(chan error, 1)
	go func() { done <- ff.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-ff.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-ff.Process.Pid, syscall.SIGKILL)
			<-done
		}
		if t.Failed() {
			t.Logf("firefox output:\n%s", log.String())
		}
	})
	return done
}

func screenshot(ctx context.Context, t *testing.T, b *broker.Broker, tabID int) {
	t.Helper()
	var shot struct {
		DataURL string `json:"data_url"`
	}
	call(ctx, t, b, "screenshot", map[string]any{keyTab: tabID}, &shot)
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(shot.DataURL, prefix) {
		t.Fatalf("screenshot is not a PNG data URL: %.40q", shot.DataURL)
	}
	raw, err := base64.StdEncoding.DecodeString(shot.DataURL[len(prefix):])
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() < 100 || img.Bounds().Dy() < 100 {
		t.Fatalf("screenshot is %v", img.Bounds())
	}
	t.Logf("screenshot: %dx%d PNG, %d bytes", img.Bounds().Dx(), img.Bounds().Dy(), len(raw))
	if out := os.Getenv("HERMES_BROWSER_E2E_OUT"); out != "" {
		writeFile(t, filepath.Join(out, "example.png"), raw)
	}
}

// formFlow types into a form on a local page and clicks its button.
func formFlow(ctx context.Context, t *testing.T, b *broker.Broker, tabID int, base string) {
	t.Helper()
	if _, err := b.Call(ctx, "navigate", map[string]any{keyTab: tabID, keyURL: base + "/"}); err == nil ||
		!strings.Contains(err.Error(), "needs the user's approval") {
		t.Errorf("navigate to another site without approval: err = %v", err)
	}
	call(ctx, t, b, "navigate", map[string]any{keyTab: tabID, keyURL: base + "/",
		keyGuard: map[string]any{keyVisit: []string{loopback}}}, nil)
	var snap snapResult
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID, keyInteractive: true}, &snap)
	t.Logf("form snapshot:\n%s", snap.Snapshot)
	r := refs(snap.Snapshot)
	if r["textbox"] == "" || r["button"] == "" {
		t.Fatalf("form snapshot lacks a textbox or button: %v", r)
	}
	call(ctx, t, b, "type", map[string]any{keyTab: tabID, keyRef: r["textbox"], keyText: "hello hermes"}, nil)
	var info inspectResult
	call(ctx, t, b, "inspect", map[string]any{keyTab: tabID, keyRef: r["button"]}, &info)
	if info.Target.Kind != "submit" || info.Target.Host != loopback || len(info.Sensitive) != 0 {
		t.Errorf("inspect of the submit button = %+v", info)
	}
	if _, err := b.Call(ctx, "click", map[string]any{keyTab: tabID, keyRef: r["button"]}); err == nil ||
		!strings.Contains(err.Error(), "needs the user's approval") {
		t.Errorf("submit without approval: err = %v", err)
	}
	call(ctx, t, b, "click", map[string]any{keyTab: tabID, keyRef: r["button"],
		keyGuard: map[string]any{"submit_hosts": []string{loopback}}}, nil)
	var text textResult
	call(ctx, t, b, "read_text", map[string]any{keyTab: tabID}, &text)
	t.Logf("after submit: %s %q", text.URL, text.Text)
	if text.Text != "got: hello hermes" {
		t.Errorf("form result = %q", text.Text)
	}

	// A link from an agent tab to a loopback name that is not allowed is
	// blocked by the extension's request filter.
	call(ctx, t, b, "back", map[string]any{keyTab: tabID}, nil)
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID, keyInteractive: true}, &snap)
	var after tabResult
	call(ctx, t, b, "click", map[string]any{keyTab: tabID, keyRef: refs(snap.Snapshot)["link"],
		keyGuard: map[string]any{keyVisit: []string{"localhost"}}}, &after)
	res, err := b.Call(ctx, "read_text", map[string]any{keyTab: tabID})
	t.Logf("after clicking a localhost link: url=%s read_text=%s err=%v", after.URL, res, err)
	if strings.Contains(string(res), "got: leak") {
		t.Error("the agent tab reached a loopback host that is not allowed")
	}
	for _, u := range []string{"http://10.0.0.1/", "http://[::ffff:127.0.0.1]/", "http://[::ffff:a00:1]/"} {
		host, _ := strings.CutPrefix(strings.TrimSuffix(u, "/"), "http://")
		if _, err := b.Call(ctx, "navigate", map[string]any{keyTab: tabID, keyURL: u,
			keyGuard: map[string]any{keyVisit: []string{host}}}); err == nil {
			t.Errorf("navigate to the private address %s was allowed", u)
		} else {
			t.Logf("navigate to %s refused: %v", u, err)
		}
	}

	// A submit button inside a link is a submit, and a button outside its
	// form (form=) still counts as that form's submit.
	call(ctx, t, b, "navigate", map[string]any{keyTab: tabID, keyURL: base + "/tricks"}, nil)
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID, keyInteractive: true}, &snap)
	t.Logf("tricks snapshot:\n%s", snap.Snapshot)
	for _, name := range []string{`- button "Inside"`, `- button "Outside"`} {
		ref := refByName(snap.Snapshot, name)
		call(ctx, t, b, "inspect", map[string]any{keyTab: tabID, keyRef: ref}, &info)
		if info.Target.Kind != "submit" {
			t.Errorf("button %s: target = %+v", name, info.Target)
		}
		if _, err := b.Call(ctx, "click", map[string]any{keyTab: tabID, keyRef: ref}); err == nil {
			t.Errorf("button %s submitted without approval", name)
		}
	}

	// A page with a password field needs the user's confirmation for
	// every action, even typing.
	call(ctx, t, b, "navigate", map[string]any{keyTab: tabID, keyURL: base + "/login"}, nil)
	call(ctx, t, b, "snapshot", map[string]any{keyTab: tabID, keyInteractive: true}, &snap)
	r = refs(snap.Snapshot)
	call(ctx, t, b, "inspect", map[string]any{keyTab: tabID}, &info)
	if strings.Join(info.Sensitive, ",") != "one_time_code,password" {
		t.Errorf("sensitive on the login page = %v", info.Sensitive)
	}
	if _, err := b.Call(ctx, "type", map[string]any{keyTab: tabID, keyRef: r["textbox"], keyText: "alice"}); err == nil ||
		!strings.Contains(err.Error(), "must confirm") {
		t.Errorf("type on a password page without confirmation: err = %v", err)
	}
	call(ctx, t, b, "type", map[string]any{keyTab: tabID, keyRef: r["textbox"], keyText: "alice",
		keyGuard: map[string]any{"sensitive": true}}, nil)
	if strings.Contains(snap.Snapshot, "hunter2") {
		t.Error("the snapshot shows a password value")
	}
}

func formPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	switch r.URL.Path {
	case "/done":
		_, _ = fmt.Fprintf(w, "<title>done</title><p>got: %s</p>", html.EscapeString(r.URL.Query().Get("q")))
	case "/tricks":
		_, _ = io.WriteString(w, `<title>tricks</title><form id="f" action="/done"><input name="q" value="x"></form>`+
			`<a href="#"><button form="f">Inside</button></a><button form="f">Outside</button>`)
	case "/login":
		_, _ = io.WriteString(w, `<title>login</title><form action="/done"><label for="u">User</label><input id="u" name="q">`+
			`<input type="password" name="pw" value="hunter2"><input name="otp" autocomplete="one-time-code">`+
			`<button>Sign in</button></form>`)
	default:
		// The link points at the same server by another loopback name,
		// which the device did not allow.
		port := r.Host[strings.LastIndex(r.Host, ":"):]
		page := `<title>form</title><form action="/done"><label for="q">Query</label>` +
			`<input id="q" name="q"><button>Go</button></form><a href="http://localhost` + port + `/done?q=leak">other</a>`
		_, _ = io.WriteString(w, page) //nolint:gosec // fixed test page
	}
}

// refusals checks that the extension acts only on its own tabs and only on
// http and https URLs.
func refusals(ctx context.Context, t *testing.T, b *broker.Broker, tabID int) {
	t.Helper()
	if _, err := b.Call(ctx, "read_text", map[string]any{keyTab: 1}); err == nil ||
		!strings.Contains(err.Error(), "not in the Hermes tab group") {
		t.Errorf("read_text on a user tab: err = %v", err)
	} else {
		t.Logf("read_text on tab 1 refused: %v", err)
	}
	if _, err := b.Call(ctx, "navigate", map[string]any{keyTab: tabID, keyURL: "file:///etc/passwd"}); err == nil {
		t.Error("navigate to file: was allowed")
	} else {
		t.Logf("navigate to file: refused: %v", err)
	}
}

// refByName returns the ref of the first snapshot line that contains name.
func refByName(snapshot, name string) string {
	for line := range strings.SplitSeq(snapshot, "\n") {
		if i := strings.LastIndex(line, "[ref="); i >= 0 && strings.Contains(line, name) {
			return strings.TrimSuffix(line[i+len("[ref="):], "]")
		}
	}
	return ""
}

// refs maps each role in a snapshot to the ref of its last element.
func refs(snapshot string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(snapshot, "\n") {
		f := strings.Fields(line)
		i := strings.LastIndex(line, "[ref=")
		if len(f) < 2 || i < 0 {
			continue
		}
		out[f[1]] = strings.TrimSuffix(line[i+len("[ref="):], "]")
	}
	return out
}

func call(ctx context.Context, t *testing.T, b *broker.Broker, op string, args, out any) {
	t.Helper()
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	res, err := b.Call(cctx, op, args)
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	if out != nil {
		if err := json.Unmarshal(res, out); err != nil {
			t.Fatalf("%s: %v: %s", op, err, res)
		}
	}
}

func waitBiDiPort(ctx context.Context, t *testing.T, profile string, ffDone <-chan error) int {
	t.Helper()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		data, err := os.ReadFile(filepath.Join(profile, "WebDriverBiDiServer.json"))
		if err == nil {
			var srv struct {
				Port int `json:"ws_port"`
			}
			if json.Unmarshal(data, &srv) == nil && srv.Port > 0 {
				return srv.Port
			}
		}
		select {
		case err := <-ffDone:
			t.Fatalf("firefox exited before the BiDi server started: %v", err)
		case <-ctx.Done():
			t.Fatal("BiDi server did not start")
		case <-tick.C:
		}
	}
}

func evalInTab(ctx context.Context, bc *bidi.Client, urlPart, expr string) string {
	res, err := bc.Call(ctx, "browsingContext.getTree", map[string]any{})
	if err != nil {
		return err.Error()
	}
	var tree struct {
		Contexts []struct {
			Context string `json:"context"`
			URL     string `json:"url"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal(res, &tree); err != nil {
		return err.Error()
	}
	for _, c := range tree.Contexts {
		if !strings.Contains(c.URL, urlPart) {
			continue
		}
		out, err := bc.Call(ctx, "script.evaluate", map[string]any{
			"expression": expr, "awaitPromise": false, "target": map[string]any{"context": c.Context},
		})
		if err != nil {
			return err.Error()
		}
		return string(out)
	}
	return "no context for " + urlPart
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // test-controlled path
		t.Fatal(err)
	}
}
