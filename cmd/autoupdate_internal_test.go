package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/USA-RedDragon/talaria/internal/autoupdate"
	"github.com/USA-RedDragon/talaria/internal/config"
	"github.com/USA-RedDragon/talaria/internal/setup"
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
	if err := os.WriteFile(bin, []byte(version), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o700); err != nil {
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

func TestDaemonFirstCheckIgnoresTheCacheOfSetup(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"tag_name":"v0.15.0"}`))
	}))
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "release.json")
	b, _ := autoBase(t, verNow, func(context.Context, string, string, []byte) (string, error) { return "x", nil })
	b.releaseLookup = func(string) *autoupdate.Releases {
		return &autoupdate.Releases{APIURL: srv.URL, Owner: "alice", Repo: "talaria", Client: srv.Client(), Path: path, Source: sourceCLI}
	}
	setupSide := b.releaseLookup("")
	if _, err := setupSide.Latest(t.Context(), autoupdate.NoticeMaxAge); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig(t, map[string]string{"TALARIA_HUB_URL": "https://hub.example.com"})
	d := b.daemonUpdater(cfg)
	if d == nil {
		t.Fatal("no daemon updater")
	}
	if tag, err := d.Latest(t.Context()); err != nil || tag != "v0.15.0" || requests.Load() != 2 {
		t.Fatalf("first check = %q, %v after %d requests; want a new lookup", tag, err, requests.Load())
	}
	if _, err := d.Latest(t.Context()); err != nil || requests.Load() != 2 {
		t.Errorf("the next check asked again: %d requests, %v", requests.Load(), err)
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
