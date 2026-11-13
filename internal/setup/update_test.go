package setup_test

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
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/setup"
)

const (
	newBody = "new"
	oldBody = "old"
	tagNew  = "v0.9.0"
)

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
	name := "talaria_0.9.0_linux_amd64.tar.gz"
	if sum == "" {
		h := sha256.Sum256(archive)
		sum = hex.EncodeToString(h[:])
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/r/latest":
			http.Redirect(w, r, "https://github.com/alice/talaria/releases/tag/v0.9.0", http.StatusFound)
		case "/r/download/v0.9.0/talaria_0.9.0_checksums.txt":
			_, _ = w.Write([]byte(sum + "  " + name + "\n" + strings.Repeat("0", 64) + "  talaria_0.9.0_darwin_arm64.tar.gz\n"))
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
			return []byte("talaria version " + version + " - abc1234\n"), nil
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
	target := filepath.Join(sys.Home, ".talaria", "bin", "talaria")
	writeFile(t, target, oldBody)
	return &setup.UpdateOptions{
		Target: target, Version: version, GOOS: linux, GOARCH: "amd64", Restart: true, Releases: rel,
		Attestations: func(context.Context, string, string, []byte) (string, error) { return "the release workflow", nil },
	}
}

func TestUpdateInstallsVerifiedRelease(t *testing.T) {
	t.Parallel()
	r := versionRunner(vNew)
	sys := newSystem(t, linux, r, map[string]string{"systemctl": "/usr/bin/" + systemctl}, nil)
	writeFile(t, filepath.Join(sys.Home, ".config", "systemd", "user", setup.UnitName), "unit")
	rel := releaseServer(t, tarball(t, map[string]string{"README.md": "x", binName: newBody}), "")
	o := updateOpts(t, sys, rel, vOld)
	tag, err := sys.Update(t.Context(), o)
	if err != nil || tag != tagNew {
		t.Fatalf("Update = %q, %v", tag, err)
	}
	if got := readFile(t, o.Target); got != newBody {
		t.Errorf("target = %q", got)
	}
	if !r.has("systemctl --user --no-block restart " + setup.UnitName) {
		t.Errorf("calls %q", r.Calls())
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(o.Target), ".talaria.new-*"))
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
		"no binary":     {archive: func(t *testing.T) []byte { t.Helper(); return tarball(t, map[string]string{"x": "y"}) }, reported: vNew, current: vOld, wantText: "has no talaria"},
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
			if got := readFile(t, o.Target); got != oldBody {
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
		{vOld, linux, "amd64", "", "talaria_0.8.1_linux_amd64.tar.gz"},
		{vOld, linux, "arm", "7", "talaria_0.8.1_linux_armv7.tar.gz"},
		{vOld, "darwin", "arm64", "", "talaria_0.8.1_darwin_arm64.tar.gz"},
	} {
		if got := setup.ArchiveName(tc[0], tc[1], tc[2], tc[3]); got != tc[4] {
			t.Errorf("ArchiveName = %q, want %q", got, tc[4])
		}
	}
	if setup.ChecksumsName(vOld) != "talaria_0.8.1_checksums.txt" || setup.LegacyChecksumsName(vOld) != "hermes-device_0.8.1_checksums.txt" || setup.XPIName(vOld) != "talaria-firefox-0.8.1.xpi" {
		t.Error("asset names")
	}
}

func TestVersions(t *testing.T) {
	t.Parallel()
	if v := setup.ParseVersion("talaria version 0.8.1 - abc1234\n"); v != vOld {
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

func TestLatestTagFollowsARename(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/alice/old/releases/latest":
			http.Redirect(w, r, "/alice/new/releases/latest", http.StatusMovedPermanently)
		case "/alice/new/releases/latest":
			http.Redirect(w, r, "/alice/new/releases/tag/v0.19.3", http.StatusFound)
		}
	}))
	defer srv.Close()
	rel := &setup.Releases{BaseURL: srv.URL + "/alice/old/releases", Client: srv.Client()}
	if tag, err := rel.LatestTag(t.Context()); err != nil || tag != "v0.19.3" {
		t.Errorf("LatestTag = %q, %v", tag, err)
	}
}

func TestUpdateVerifiesTheArchiveAttestation(t *testing.T) {
	t.Parallel()
	archive := tarball(t, map[string]string{binName: newBody})
	sys := newSystem(t, linux, versionRunner(vNew), nil, nil)
	o := updateOpts(t, sys, releaseServer(t, archive, ""), vOld)
	var gotTag, gotName string
	var gotDigest []byte
	o.Attestations = func(_ context.Context, tag, name string, digest []byte) (string, error) {
		gotTag, gotName, gotDigest = tag, name, digest
		return "the release workflow", nil
	}
	if _, err := sys.Update(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(archive)
	if gotTag != tagNew || gotName != "talaria_0.9.0_linux_amd64.tar.gz" || !bytes.Equal(gotDigest, want[:]) {
		t.Errorf("verified %q %q %x, want v0.9.0 %x", gotTag, gotName, gotDigest, want)
	}
	if readFile(t, o.Target) != newBody {
		t.Error("not installed")
	}
}

func TestUpdateRefusesUnverifiedArchive(t *testing.T) {
	t.Parallel()
	errBad := errors.New("no attestation")
	for name, tc := range map[string]struct {
		tag      string
		verifier setup.AttestationVerifier
		wantText string
	}{
		"failed":         {verifier: func(context.Context, string, string, []byte) (string, error) { return "", errBad }, wantText: "older than v" + setup.FirstAttestedVersion},
		"failed, new":    {tag: setup.FirstAttestedVersion, verifier: func(context.Context, string, string, []byte) (string, error) { return "", errBad }, wantText: "was not installed"},
		"no verifier":    {wantText: "no attestation verifier"},
		"failed, newest": {tag: "99.0.0", verifier: func(context.Context, string, string, []byte) (string, error) { return "", errBad }, wantText: "--insecure-skip-attestation"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := vNew
			if tc.tag != "" {
				want = tc.tag
			}
			archive := tarball(t, map[string]string{binName: newBody})
			sys := newSystem(t, linux, versionRunner(want), nil, nil)
			rel := releaseServer(t, archive, "")
			if tc.tag != "" {
				rel = taggedReleaseServer(t, tc.tag, archive)
			}
			o := updateOpts(t, sys, rel, vOld)
			o.Tag, o.Attestations = tc.tag, tc.verifier
			_, err := sys.Update(t.Context(), o)
			if !errors.Is(err, setup.ErrAttestation) || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("err = %v", err)
			}
			if tc.verifier != nil && !errors.Is(err, errBad) {
				t.Errorf("err = %v does not wrap the cause", err)
			}
			if got := readFile(t, o.Target); got != oldBody {
				t.Errorf("target replaced: %q", got)
			}
		})
	}
}

func TestUpdateInsecureSkipAttestation(t *testing.T) {
	t.Parallel()
	sys := newSystem(t, linux, versionRunner(vNew), nil, nil)
	var out bytes.Buffer
	sys.Out = &out
	o := updateOpts(t, sys, releaseServer(t, tarball(t, map[string]string{binName: newBody}), ""), vOld)
	o.SkipAttestation = true
	o.Attestations = func(context.Context, string, string, []byte) (string, error) {
		t.Error("verified with --insecure-skip-attestation")
		return "", nil
	}
	if _, err := sys.Update(t.Context(), o); err != nil {
		t.Fatal(err)
	}
	if readFile(t, o.Target) != newBody || !strings.Contains(out.String(), "WARNING: not verifying") {
		t.Errorf("target %q, output %q", readFile(t, o.Target), out.String())
	}
}

func TestUpdateCheckInstallsNothing(t *testing.T) {
	t.Parallel()
	r := versionRunner(vNew)
	sys := newSystem(t, linux, r, map[string]string{"systemctl": "/usr/bin/" + systemctl}, nil)
	writeFile(t, filepath.Join(sys.Home, ".config", "systemd", "user", setup.UnitName), "unit")
	o := updateOpts(t, sys, releaseServer(t, tarball(t, map[string]string{binName: newBody}), ""), vNew)
	o.Check = true
	tag, err := sys.Update(t.Context(), o)
	if err != nil || tag != tagNew {
		t.Fatalf("Update = %q, %v", tag, err)
	}
	if readFile(t, o.Target) != oldBody || r.has("systemctl --user --no-block restart "+setup.UnitName) {
		t.Errorf("--check changed something: %q %q", readFile(t, o.Target), r.Calls())
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(o.Target), ".talaria.new-*"))
	if len(leftovers) != 0 {
		t.Errorf("left %q", leftovers)
	}
}

// taggedReleaseServer serves release tag (without v) with archive for
// linux/amd64.
func taggedReleaseServer(t *testing.T, version string, archive []byte) *setup.Releases {
	t.Helper()
	name := setup.ArchiveName(version, linux, "amd64", "")
	h := sha256.Sum256(archive)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/r/download/v" + version + "/" + setup.ChecksumsName(version):
			_, _ = w.Write([]byte(hex.EncodeToString(h[:]) + "  " + name + "\n"))
		case "/r/download/v" + version + "/" + name:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &setup.Releases{BaseURL: srv.URL + "/r", Client: srv.Client()}
}

func TestUpdateCheckRefusesSkip(t *testing.T) {
	t.Parallel()
	sys := newSystem(t, linux, versionRunner(vNew), nil, nil)
	o := updateOpts(t, sys, releaseServer(t, tarball(t, map[string]string{binName: newBody}), ""), vOld)
	o.Check, o.SkipAttestation = true, true
	if _, err := sys.Update(t.Context(), o); err == nil || !strings.Contains(err.Error(), "--insecure-skip-attestation") {
		t.Fatalf("err = %v", err)
	}
}
