package setup_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-device/internal/setup"
)

const newBody = "new"

func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// releaseServer serves release v0.9.0 with the given archive for
// linux/amd64 and a checksum file that lists sum for it.
func releaseServer(t *testing.T, archive []byte, sum string) *setup.Releases {
	t.Helper()
	name := "hermes-device_0.9.0_linux_amd64.tar.gz"
	if sum == "" {
		h := sha256.Sum256(archive)
		sum = hex.EncodeToString(h[:])
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/r/latest":
			http.Redirect(w, r, "https://github.com/alice/hermes-device/releases/tag/v0.9.0", http.StatusFound)
		case "/r/download/v0.9.0/hermes-device_0.9.0_checksums.txt":
			_, _ = w.Write([]byte(sum + "  " + name + "\n" + strings.Repeat("0", 64) + "  hermes-device_0.9.0_darwin_arm64.tar.gz\n"))
		case "/r/download/v0.9.0/" + name:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &setup.Releases{BaseURL: srv.URL + "/r", Client: srv.Client()}
}

func versionRunner(version string) *fakeRunner {
	return &fakeRunner{respond: func(name string, args []string) ([]byte, error) {
		switch {
		case len(args) == 1 && args[0] == "--version":
			return []byte("hermes-device version " + version + " - abc1234\n"), nil
		case name == systemctl && len(args) > 1 && args[1] == "is-system-running":
			return []byte("running\n"), nil
		case name == systemctl && len(args) > 1 && args[1] == "is-active":
			return []byte("active\n"), nil
		}
		return nil, nil
	}}
}

func updateOpts(t *testing.T, sys *setup.System, rel *setup.Releases, version string) *setup.UpdateOptions {
	t.Helper()
	target := filepath.Join(sys.Home, ".hermes-device", "bin", "hermes-device")
	writeFile(t, target, "old")
	return &setup.UpdateOptions{Target: target, Version: version, GOOS: linux, GOARCH: "amd64", Restart: true, Releases: rel}
}

func TestUpdateInstallsVerifiedRelease(t *testing.T) {
	t.Parallel()
	r := versionRunner(vNew)
	sys := newSystem(t, linux, r, map[string]string{"systemctl": "/usr/bin/" + systemctl}, nil)
	writeFile(t, filepath.Join(sys.Home, ".config", "systemd", "user", setup.UnitName), "unit")
	rel := releaseServer(t, tarball(t, map[string]string{"README.md": "x", binName: newBody}), "")
	o := updateOpts(t, sys, rel, vOld)
	tag, err := sys.Update(t.Context(), o)
	if err != nil || tag != "v0.9.0" {
		t.Fatalf("Update = %q, %v", tag, err)
	}
	if got := readFile(t, o.Target); got != newBody {
		t.Errorf("target = %q", got)
	}
	if !r.has("systemctl --user --no-block restart " + setup.UnitName) {
		t.Errorf("calls %q", r.Calls())
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(o.Target), ".hermes-device.new-*"))
	if len(leftovers) != 0 {
		t.Errorf("left %q", leftovers)
	}
}

func TestUpdateRefusals(t *testing.T) {
	t.Parallel()
	good := func(t *testing.T) []byte {
		t.Helper()
		return tarball(t, map[string]string{binName: newBody})
	}
	for name, tc := range map[string]struct {
		archive  func(*testing.T) []byte
		sum      string
		reported string
		current  string
		tag      string
		goarch   string
		want     error
		wantText string
	}{
		"checksum":      {archive: good, sum: strings.Repeat("ab", 32), reported: vNew, current: vOld, want: setup.ErrChecksum},
		"wrong version": {archive: good, reported: "0.8.0", current: vOld, wantText: `reports version "0.8.0"`},
		"downgrade":     {archive: good, reported: vNew, current: "0.10.0", want: setup.ErrDowngrade},
		"same":          {archive: good, reported: vNew, current: vNew, want: setup.ErrUpToDate},
		"dev build":     {archive: good, reported: vNew, current: "dev", want: setup.ErrDevBuild},
		"no binary":     {archive: func(t *testing.T) []byte { t.Helper(); return tarball(t, map[string]string{"x": "y"}) }, reported: vNew, current: vOld, wantText: "has no hermes-device"},
		"no arch":       {archive: good, reported: vNew, current: vOld, goarch: "riscv64", want: setup.ErrNoAsset},
		"bad tag":       {archive: good, reported: vNew, current: vOld, tag: "latest", wantText: "not a release version"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sys := newSystem(t, linux, versionRunner(tc.reported), nil, nil)
			o := updateOpts(t, sys, releaseServer(t, tc.archive(t), tc.sum), tc.current)
			o.Tag = tc.tag
			if tc.goarch != "" {
				o.GOARCH = tc.goarch
			}
			_, err := sys.Update(t.Context(), o)
			if tc.want != nil && !errors.Is(err, tc.want) || tc.wantText != "" && (err == nil || !strings.Contains(err.Error(), tc.wantText)) {
				t.Fatalf("err = %v", err)
			}
			if got := readFile(t, o.Target); got != "old" {
				t.Errorf("target replaced: %q", got)
			}
		})
	}
}

func TestUpdateForceAllowsDowngrade(t *testing.T) {
	t.Parallel()
	sys := newSystem(t, linux, versionRunner(vNew), nil, nil)
	o := updateOpts(t, sys, releaseServer(t, tarball(t, map[string]string{binName: newBody}), ""), "1.0.0")
	o.Tag, o.Force = vNew, true
	if _, err := sys.Update(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(o.Target); err != nil || fi.Mode().Perm() != 0o755 || readFile(t, o.Target) != newBody {
		t.Errorf("target %v %v", fi, err)
	}
}

func TestArchiveNames(t *testing.T) {
	t.Parallel()
	for _, tc := range [][5]string{
		{vOld, linux, "amd64", "", "hermes-device_0.8.1_linux_amd64.tar.gz"},
		{vOld, linux, "arm", "7", "hermes-device_0.8.1_linux_armv7.tar.gz"},
		{vOld, "darwin", "arm64", "", "hermes-device_0.8.1_darwin_arm64.tar.gz"},
	} {
		if got := setup.ArchiveName(tc[0], tc[1], tc[2], tc[3]); got != tc[4] {
			t.Errorf("ArchiveName = %q, want %q", got, tc[4])
		}
	}
	if setup.ChecksumsName(vOld) != "hermes-device_0.8.1_checksums.txt" || setup.XPIName(vOld) != "hermes-device-firefox-0.8.1.xpi" {
		t.Error("asset names")
	}
}

func TestVersions(t *testing.T) {
	t.Parallel()
	if v := setup.ParseVersion("hermes-device version 0.8.1 - abc1234\n"); v != vOld {
		t.Errorf("ParseVersion = %q", v)
	}
	if v := setup.ParseVersion("garbage"); v != "" {
		t.Errorf("ParseVersion = %q", v)
	}
	for _, tc := range []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"0.10.0", vNew, 1, true}, {"v0.9.0", vNew, 0, true}, {"0.9.0-rc1", vNew, -1, true}, {"dev", vNew, 0, false},
	} {
		if c, ok := setup.Compare(tc.a, tc.b); c != tc.cmp || ok != tc.ok {
			t.Errorf("Compare(%q, %q) = %d, %t", tc.a, tc.b, c, ok)
		}
	}
}

func TestLatestTagNeedsRedirect(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/r/latest" {
			http.Redirect(w, r, "/r/tag/not-a-version", http.StatusFound)
		}
	}))
	defer srv.Close()
	rel := &setup.Releases{BaseURL: srv.URL + "/r", Client: srv.Client()}
	if _, err := rel.LatestTag(t.Context()); err == nil {
		t.Error("accepted a redirect to no release")
	}
}
