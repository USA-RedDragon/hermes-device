package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/configulator/v2"
	"github.com/USA-RedDragon/hermes-device/internal/autoupdate"
	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/USA-RedDragon/hermes-device/internal/setup"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

const (
	tagNext = "v0.15.0"
	verNow  = "0.14.0"
)

// fileRunner answers <path> --version with the version written in the
// file, the way a release binary would.
// fileRunner runs a fake binary whose file holds its version. It passes
// run --check-config unless rejectConfig is set.
type fileRunner struct{ rejectConfig bool }

func (f fileRunner) Output(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
	data, err := os.ReadFile(name)
	if err == nil && len(args) >= 2 && args[0] == runCommand && args[len(args)-1] == "--"+CheckConfigFlag {
		if f.rejectConfig {
			return []byte("unknown key"), errNoProgram
		}
		return nil, nil
	}
	if err != nil || len(args) != 1 || args[0] != "--version" {
		return nil, errNoProgram
	}
	return []byte("talaria version " + strings.TrimSpace(string(data)) + " - abc\n"), nil
}

func (fileRunner) Start(string, ...string) error { return errNoProgram }

func targz(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// autoBase is a base with a home that holds an installed binary of
// version, which this process runs, and a release server with v0.15.0.
func autoBase(t *testing.T, version string, verify setup.AttestationVerifier) (*base, string) {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, setup.BinDir, setup.BinaryName)
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte(version), 0o700); err != nil { //nolint:gosec // a program
		t.Fatal(err)
	}
	archive := targz(t, setup.BinaryName, "0.15.0")
	sum := sha256.Sum256(archive)
	name := setup.ArchiveName("0.15.0", runtime.GOOS, runtime.GOARCH, goarm())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/r/download/v0.15.0/" + setup.ChecksumsName("0.15.0"):
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + name + "\n"))
		case "/r/download/v0.15.0/" + name:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	b := &base{
		version: version,
		system: func() (*setup.System, error) {
			return &setup.System{GOOS: runtime.GOOS, Home: home, Run: fileRunner{}, Getenv: func(string) string { return "" },
				Out: &bytes.Buffer{}, Now: time.Now}, nil
		},
		releases:      func() *setup.Releases { return &setup.Releases{BaseURL: srv.URL + "/r", Client: srv.Client()} },
		verifier:      func() setup.AttestationVerifier { return verify },
		releaseLookup: func(string) *autoupdate.Releases { return &autoupdate.Releases{} },
		executable:    func() (string, error) { return bin, nil },
		underService:  func() bool { return true },
		exit:          func(int) { t.Error("exit") },
		getenv:        func(string) string { return "" },
	}
	return b, bin
}

func defaultConfig(t *testing.T, env map[string]string) *config.Config {
	t.Helper()
	cfg, err := configulator.New(config.ConfigSchema()).
		WithEnvironmentVariables(&configulator.EnvironmentVariableOptions{Prefix: config.EnvPrefix, Separator: config.EnvSeparator}).
		WithEnviron(func(k string) (string, bool) { v, ok := env[k]; return v, ok }).
		Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAutoInstallRefusesAReleaseThatRejectsTheConfig(t *testing.T) {
	t.Parallel()
	ok := func(context.Context, string, string, []byte) (string, error) { return "the release workflow", nil }
	b, bin := autoBase(t, verNow, ok)
	sys, _ := b.system()
	sys.Run = fileRunner{rejectConfig: true}
	b.runArgs = []string{runCommand, "-c", "/x/config.yaml"}
	err := b.autoInstall(t.Context(), sys, bin, tagNext)
	if err == nil || !strings.Contains(err.Error(), `["run" "-c" "/x/config.yaml" "--check-config"]`) {
		t.Fatalf("autoInstall = %v", err)
	}
	if data, _ := os.ReadFile(bin); string(data) != verNow {
		t.Errorf("installed %q", data)
	}
}

func TestRunArgs(t *testing.T) {
	t.Parallel()
	if got := runArgs([]string{"/bin/talaria", runCommand, "-c", "a.yaml"}); strings.Join(got, " ") != "run -c a.yaml" {
		t.Errorf("runArgs = %q", got)
	}
	if got := runArgs([]string{"cmd.test", "-test.v"}); strings.Join(got, " ") != runCommand {
		t.Errorf("runArgs = %q", got)
	}
}

func TestAutoInstallVerifies(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	ok := func(_ context.Context, tag, name string, digest []byte) (string, error) {
		calls.Add(1)
		if tag != tagNext || !strings.HasPrefix(name, "talaria_0.15.0_") || len(digest) != 32 {
			t.Errorf("verifier got %s %s %x", tag, name, digest)
		}
		return "the release workflow", nil
	}
	b, bin := autoBase(t, verNow, ok)
	sys, _ := b.system()
	if err := b.autoInstall(t.Context(), sys, bin, tagNext); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(bin); string(data) != "0.15.0" || calls.Load() != 1 {
		t.Errorf("binary %q after %d verifications", data, calls.Load())
	}
	if err := b.autoInstall(t.Context(), sys, bin, tagNext); !errors.Is(err, setup.ErrUpToDate) {
		t.Errorf("reinstall: %v", err)
	}
}

func TestAutoInstallVerificationFailureBlocksTheInstall(t *testing.T) {
	t.Parallel()
	bad := func(context.Context, string, string, []byte) (string, error) {
		return "", errors.New("no attestation of this file passed verification")
	}
	b, bin := autoBase(t, verNow, bad)
	sys, _ := b.system()
	if err := b.autoInstall(t.Context(), sys, bin, tagNext); !errors.Is(err, setup.ErrAttestation) {
		t.Fatalf("autoInstall = %v", err)
	}
	if data, _ := os.ReadFile(bin); string(data) != verNow {
		t.Errorf("binary replaced with %q", data)
	}
	entries, _ := os.ReadDir(filepath.Dir(bin))
	if len(entries) != 1 {
		t.Errorf("left files behind: %v", entries)
	}
}

func TestAutoInstallNeverDowngradesOrInstallsPrerelease(t *testing.T) {
	t.Parallel()
	ok := func(context.Context, string, string, []byte) (string, error) { return "x", nil }
	b, bin := autoBase(t, "0.16.0", ok)
	sys, _ := b.system()
	if err := b.autoInstall(t.Context(), sys, bin, tagNext); !errors.Is(err, setup.ErrDowngrade) {
		t.Errorf("downgrade: %v", err)
	}
	for _, tag := range []string{"v0.17.0-rc.1", "0.17.0", "v0.17.0+x"} {
		if err := b.autoInstall(t.Context(), sys, bin, tag); err == nil || !strings.Contains(err.Error(), "not a stable release") {
			t.Errorf("%s: %v", tag, err)
		}
	}
	if data, _ := os.ReadFile(bin); string(data) != "0.16.0" {
		t.Errorf("binary %q", data)
	}
}

func TestDaemonUpdaterDefaults(t *testing.T) {
	t.Parallel()
	ok := func(context.Context, string, string, []byte) (string, error) { return "x", nil }
	cfg := defaultConfig(t, map[string]string{"TALARIA_HUB_URL": "https://hub.example.com"})
	if !cfg.Update.Auto || cfg.Update.Interval != 6*time.Hour {
		t.Fatalf("defaults: %+v", cfg.Update)
	}
	b, _ := autoBase(t, verNow, ok)
	if d := b.daemonUpdater(cfg); d == nil || d.Interval != 6*time.Hour || d.Version != verNow {
		t.Fatalf("desktop device: %+v", d)
	}
	for name, change := range map[string]func(*base, *config.Config){
		"update.auto false": func(_ *base, c *config.Config) { c.Update.Auto = false },
		"hub.token":         func(_ *base, c *config.Config) { c.Hub.Token = "hdt_x" },
		"hub.token-file":    func(_ *base, c *config.Config) { c.Hub.TokenFile = "/run/secrets/token" },
		"dev build":         func(b *base, _ *config.Config) { b.version = "dev" },
		"no service":        func(b *base, _ *config.Config) { b.underService = func() bool { return false } },
		"other binary":      func(b *base, _ *config.Config) { b.executable = os.Executable },
	} {
		b, _ := autoBase(t, verNow, ok)
		c := *cfg
		change(b, &c)
		if d := b.daemonUpdater(&c); d != nil {
			t.Errorf("%s: auto-update on", name)
		}
	}
}

// The sandbox and device images set update.auto false; their devices run
// with hub.token-file, which also turns it off.
func TestImagesTurnAutoUpdateOff(t *testing.T) {
	t.Parallel()
	for _, f := range []string{"../Dockerfile", "../Dockerfile.sandbox"} {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "TALARIA_UPDATE_AUTO=false") {
			t.Errorf("%s does not set TALARIA_UPDATE_AUTO=false", f)
		}
	}
	cfg := defaultConfig(t, map[string]string{"TALARIA_UPDATE_AUTO": "false"})
	if cfg.Update.Auto {
		t.Error("TALARIA_UPDATE_AUTO=false left auto-update on")
	}
}

func TestInteractiveNoticeDoesNotBlock(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{"tag_name":"v0.15.0"}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(unblock)
	path := filepath.Join(t.TempDir(), "release.json")
	b, _ := autoBase(t, verNow, nil)
	b.releaseLookup = func(string) *autoupdate.Releases {
		return &autoupdate.Releases{APIURL: srv.URL, Owner: "alice", Repo: "talaria", Client: srv.Client(), Path: path}
	}
	stale := time.Now().Add(-2 * autoupdate.NoticeMaxAge).Format(time.RFC3339)
	if err := os.WriteFile(path, []byte(`{"tag":"v0.15.0","tried_at":"`+stale+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	b.noticeOut = &out
	begin := time.Now()
	b.startNotice(&config.Config{Update: config.Update{Auto: true}})
	if d := time.Since(begin); d > 500*time.Millisecond {
		t.Errorf("the notice took %s", d)
	}
	if !strings.Contains(out.String(), "v0.15.0 is available") {
		t.Errorf("notice %q", out.String())
	}
	unblock()
	b.notice.Finish()
	if _, checked := b.notice.Releases.Cached(); checked.IsZero() {
		t.Error("the background lookup did not finish")
	}
	b.notice = nil
	b.getenv = func(k string) string {
		if k == NoNoticeEnv {
			return "1"
		}
		return ""
	}
	out.Reset()
	b.startNotice(&config.Config{})
	if out.Len() != 0 || b.notice != nil {
		t.Errorf("%s did not turn the notice off: %q", NoNoticeEnv, out.String())
	}
}

// runDaemon with auto-update installs, waits until the device is idle and
// no TUI is open, and then stops with restartError.
func TestRunRestartsAfterAnUpdateWhenIdle(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	busy := make(chan struct{})
	var once sync.Once
	hub := &runHub{}
	hub.exec = func(ctx context.Context, conn *websocket.Conn) {
		writeJSONMsg(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Cmd: "echo started; sleep 30", Cwd: hub.root, Timeout: 60})
		go func() {
			<-release
			writeJSONMsg(ctx, conn, protocol.Kill{Type: protocol.TypeKill, ID: "1"})
		}()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var p protocol.Ping
			_ = json.Unmarshal(data, &p)
			switch p.Type {
			case protocol.TypePing:
				writeJSONMsg(ctx, conn, protocol.Ping{Type: protocol.TypePong, Seq: p.Seq})
			case protocol.TypeOutput:
				once.Do(func() { close(busy) })
			}
		}
	}
	s := setupRun(t, hub, nil)
	hub.root = s.cfg.Device.Roots[0]
	s.cfg.PingInterval = 50 * time.Millisecond
	var installed atomic.Int32
	s.splice.tuis.Store(1)
	s.au = &autoupdate.Daemon{
		Version: verNow, Interval: time.Hour, FirstMin: time.Millisecond, FirstMax: time.Millisecond,
		IdleWait: time.Hour, IdlePoll: 20 * time.Millisecond,
		Latest:    func(context.Context) (string, error) { return tagNext, nil },
		Installed: func(context.Context) (string, error) { return verNow, nil },
		Install: func(context.Context, string) error {
			<-busy
			installed.Add(1)
			return nil
		},
	}
	done := s.start(t.Context())
	waitUntil(t, "the install", func() bool { return installed.Load() == 1 })
	var tuiClosed atomic.Int64
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(release)
		time.Sleep(300 * time.Millisecond)
		tuiClosed.Store(time.Now().UnixNano())
		s.splice.tuis.Store(0)
	}()
	err := waitErr(t, done)
	var restart *restartError
	if !errors.As(err, &restart) || restart.tag != tagNext {
		t.Fatalf("runDaemon = %v", err)
	}
	if tuiClosed.Load() == 0 {
		t.Error("restarted while a command ran or a TUI was open")
	}
	if _, err := os.Stat(filepath.Join(s.stateDir, "daemon.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("daemon state left behind: %v", err)
	}
}
