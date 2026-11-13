//go:build unix

// Package sandboxtest_test starts the sandbox image and uses its browser the
// way the plugin does: over the exec leg, with browser requests. A fake hub
// plays the plugin. It runs only with TALARIA_SANDBOX_IMAGE set to an image
// that docker can start, for example
//
//	TALARIA_SANDBOX_IMAGE=hx-sandbox:new go test ./internal/browser/sandboxtest/ -v
//
// and needs internet access for https://example.com.
package sandboxtest_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/hubapi"
	"github.com/USA-RedDragon/talaria/protocol"
	"github.com/coder/websocket"
)

const (
	token      = "hdt_sandboxtest"
	exampleURL = "https://example.com/"
	keyTab     = "tab_id"
)

type hub struct {
	t       *testing.T
	legs    atomic.Int32
	status  chan protocol.BrowserStatus
	results chan protocol.BrowserResult
	send    chan protocol.Browser
	nextID  atomic.Int32
}

func (h *hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != hubapi.PathExecLeg {
		http.NotFound(w, r)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(64 << 20)
	ctx := r.Context()
	h.legs.Add(1)
	_, data, err := conn.Read(ctx)
	if err != nil {
		return
	}
	var hello protocol.Hello
	if json.Unmarshal(data, &hello) != nil || hello.Type != protocol.TypeHello {
		h.t.Errorf("bad hello %q", data)
		return
	}
	write := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			h.t.Error(err)
			return
		}
		_ = conn.Write(ctx, websocket.MessageText, b)
	}
	write(protocol.Welcome{Type: protocol.TypeWelcome, Version: protocol.Version, DeviceID: "d_sandbox", Name: "sandbox-test",
		PingInterval: 25, Capabilities: []string{protocol.CapabilityBrowser}})
	write(protocol.Resume{Type: protocol.TypeResume, Commands: []protocol.ResumeCommand{}})
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-h.send:
				write(req)
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var env protocol.Envelope
		_ = json.Unmarshal(data, &env)
		switch env.Type {
		case protocol.TypePing:
			write(map[string]string{"type": protocol.TypePong})
		case protocol.TypeBrowserStatus:
			var st protocol.BrowserStatus
			_ = json.Unmarshal(data, &st)
			select {
			case h.status <- st:
			default:
			}
		case protocol.TypeBrowserResult:
			var res protocol.BrowserResult
			_ = json.Unmarshal(data, &res)
			h.results <- res
		}
	}
}

func (h *hub) call(ctx context.Context, op string, args any, out any) {
	h.t.Helper()
	id := fmt.Sprintf("r%d", h.nextID.Add(1))
	raw, err := json.Marshal(args)
	if err != nil {
		h.t.Fatal(err)
	}
	h.send <- protocol.Browser{Type: protocol.TypeBrowser, ID: id, Op: op, Args: raw, Timeout: 60}
	for {
		select {
		case res := <-h.results:
			if res.ID != id {
				continue
			}
			if !res.OK {
				h.t.Fatalf("%s: %s: %s", op, res.Code, res.Error)
			}
			if out != nil {
				if err := json.Unmarshal(res.Result, out); err != nil {
					h.t.Fatalf("%s: %v: %s", op, err, res.Result)
				}
			}
			return
		case <-ctx.Done():
			h.t.Fatalf("%s: %v", op, ctx.Err())
		}
	}
}

func TestSandboxImageBrowser(t *testing.T) { //nolint:paralleltest // one container
	image := os.Getenv("TALARIA_SANDBOX_IMAGE")
	if image == "" {
		t.Skip("set TALARIA_SANDBOX_IMAGE to the sandbox image to start it")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	h := &hub{t: t, status: make(chan protocol.BrowserStatus, 16), results: make(chan protocol.BrowserResult, 16),
		send: make(chan protocol.Browser, 4)}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	name := fmt.Sprintf("talaria-sandboxtest-%d", os.Getpid())
	// The settings of the cluster pod: no root filesystem writes, no
	// capabilities, /work and /tmp the only writable places.
	args := []string{"run", "--rm", "--name", name, "--network", "host", "--read-only",
		"--tmpfs", "/tmp", "--tmpfs", "/work:uid=568,gid=568", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"-e", "TALARIA_HUB_URL=" + srv.URL, "-e", "TALARIA_HUB_TOKEN=" + token, image}
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // a test
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", name).Run() //nolint:gosec,noctx // cleanup
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("container output:\n%s", logs.String())
		}
	})

	// The daemon starts Firefox by itself; wait until the extension connects.
	deadline := time.After(3 * time.Minute)
	for connected := false; !connected; {
		select {
		case st := <-h.status:
			t.Logf("browser_status: connected=%v can_start=%v browser=%q extension=%q", st.Connected, st.CanStart, st.Browser, st.ExtensionVersion)
			connected = st.Connected
		case <-deadline:
			t.Fatal("the sandbox's Firefox did not connect to the daemon in 3 minutes")
		}
	}

	var tab struct {
		TabID   int    `json:"tab_id"`
		URL     string `json:"url"`
		Title   string `json:"title"`
		GroupID int    `json:"group_id"`
	}
	h.call(ctx, "tab_open", map[string]any{"url": exampleURL, "guard": map[string]any{"visit_hosts": []string{"example.com"}}}, &tab)
	t.Logf("tab_open: id=%d url=%s title=%q group=%d", tab.TabID, tab.URL, tab.Title, tab.GroupID)
	if tab.Title != "Example Domain" {
		t.Errorf("title = %q, want Example Domain", tab.Title)
	}

	var snap struct {
		Snapshot string `json:"snapshot"`
	}
	h.call(ctx, "snapshot", map[string]any{keyTab: tab.TabID}, &snap)
	t.Logf("snapshot:\n%s", snap.Snapshot)
	if !strings.Contains(snap.Snapshot, `- link "Learn more" [ref=`) {
		t.Errorf("snapshot lacks the link: %s", snap.Snapshot)
	}

	var shot struct {
		DataURL string `json:"data_url"`
	}
	h.call(ctx, "screenshot", map[string]any{keyTab: tab.TabID}, &shot)
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

	h.call(ctx, "tab_close", map[string]any{keyTab: tab.TabID}, nil)
}
