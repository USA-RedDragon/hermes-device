package oci_test

import (
	"archive/tar"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/oci"
	"github.com/USA-RedDragon/talaria/internal/oci/ocitest"
)

const (
	fileF     = "opt/app/f"
	appBin    = "opt/app/bin/app"
	libAlpine = "library/alpine"
	amd64     = "amd64"
)

func appFilter() oci.Filter {
	return oci.Filter{Include: []string{"opt/app", "usr/local/bin"}, Exclude: []string{"opt/app/big-*"}}
}

func env() []string { return []string{"PATH=/usr/bin", "APP_HOME=/opt/app"} }

func ref(t *testing.T, r *ocitest.Registry, digest string) oci.Reference {
	t.Helper()
	ref, err := oci.ParseReference(r.Reference(digest))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func pull(t *testing.T, r *ocitest.Registry, ref oci.Reference) (string, *oci.Image, error) {
	t.Helper()
	dest := t.TempDir()
	img, err := oci.Pull(context.Background(), ref, dest, oci.Options{HTTP: r.Client(), Arch: amd64, Filter: appFilter()})
	return dest, img, err
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPullIndexLayersAndWhiteouts(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "team/app")
	l1 := ocitest.Layer(t,
		ocitest.Entry{Name: "usr/", Type: tar.TypeDir, Mode: 0o755},
		ocitest.Entry{Name: "usr/share/doc/skip", Body: "not kept"},
		ocitest.Entry{Name: "opt/app/", Type: tar.TypeDir, Mode: 0o555},
		ocitest.Entry{Name: appBin, Body: "#!/bin/sh\necho app\n", Mode: 0o4755},
		ocitest.Entry{Name: "opt/app/old", Body: "old"},
		ocitest.Entry{Name: "opt/app/dir/x", Body: "x"},
		ocitest.Entry{Name: "opt/app/big-store/blob", Body: "excluded"},
		ocitest.Entry{Name: "opt/app/lnk", Type: tar.TypeSymlink, Link: "/usr/local/bin/app"},
		ocitest.Entry{Name: "opt/app/hard", Type: tar.TypeLink, Link: appBin},
		ocitest.Entry{Name: "usr/local/bin/app", Type: tar.TypeSymlink, Link: "/opt/app/bin/app"},
	)
	l2 := ocitest.Layer(t,
		ocitest.Entry{Name: "opt/app/.wh.old"},
		ocitest.Entry{Name: "opt/app/dir/.wh..wh..opq"},
		ocitest.Entry{Name: "opt/app/dir/y", Body: "y"},
		ocitest.Entry{Name: appBin, Body: "#!/bin/sh\necho new\n", Mode: 0o755},
	)
	img := r.Push(t, oci.MediaOCILayerGzip, env(), l1, l2)
	dest, got, err := pull(t, r, ref(t, r, img.Index))
	if err != nil {
		t.Fatal(err)
	}
	if got.Manifest != img.Manifest || got.Source.Domain != r.Domain() {
		t.Errorf("image = %+v", got)
	}
	if len(got.Config.Config.Env) != 2 {
		t.Errorf("env = %v", got.Config.Config.Env)
	}
	if s := readFile(t, filepath.Join(dest, appBin)); !strings.Contains(s, "new") {
		t.Errorf("app = %q", s)
	}
	fi, err := os.Stat(filepath.Join(dest, appBin))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetuid != 0 || fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if s := readFile(t, filepath.Join(dest, "opt/app/hard")); !strings.Contains(s, "echo app") {
		t.Errorf("hard link = %q", s)
	}
	if l, err := os.Readlink(filepath.Join(dest, "opt/app/lnk")); err != nil || l != "/usr/local/bin/app" {
		t.Errorf("symlink = %q, %v", l, err)
	}
	for _, gone := range []string{"opt/app/old", "opt/app/dir/x", "usr/share", "opt/app/big-store"} {
		if _, err := os.Lstat(filepath.Join(dest, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists: %v", gone, err)
		}
	}
	if s := readFile(t, filepath.Join(dest, "opt/app/dir/y")); s != "y" {
		t.Errorf("y = %q", s)
	}
	if n := r.Tokens(); n != 1 {
		t.Errorf("tokens = %d, want 1", n)
	}
}

func TestPullRefusesDigestMismatch(t *testing.T) {
	t.Parallel()
	for _, what := range []string{"index", "manifest", "config", "layer"} {
		t.Run(what, func(t *testing.T) {
			t.Parallel()
			r := ocitest.New(t, "team/app")
			l := ocitest.Layer(t, ocitest.Entry{Name: fileF, Body: "good"})
			img := r.Push(t, oci.MediaOCILayerGzip, env(), l)
			bad := ocitest.Layer(t, ocitest.Entry{Name: fileF, Body: "evil"})
			switch what {
			case "index":
				r.Tamper(img.Index, []byte(`{"schemaVersion":2,"manifests":[]}`))
			case "manifest":
				r.Tamper(img.Manifest, []byte(`{"schemaVersion":2}`))
			case "config":
				r.Tamper(img.Config, []byte(`{"os":"linux","architecture":"amd64"}`))
			case "layer":
				r.Tamper(img.Layers[0], bad)
			}
			_, _, err := pull(t, r, ref(t, r, img.Index))
			if !errors.Is(err, oci.ErrDigest) {
				t.Fatalf("err = %v, want ErrDigest", err)
			}
		})
	}
}

func TestPullRefusesTraversal(t *testing.T) {
	t.Parallel()
	for name, l := range map[string][]ocitest.Entry{
		"dotdot": {{Name: "opt/app/../../escape", Body: "x"}},
		"through symlink": {
			{Name: "opt/app/out", Type: tar.TypeSymlink, Link: "../../.."},
			{Name: "opt/app/out/escape", Body: "x"},
		},
		"absolute symlink": {
			{Name: "opt/app/abs", Type: tar.TypeSymlink, Link: "/"},
			{Name: "opt/app/abs/escape", Body: "x"},
		},
		"hard link":            {{Name: "opt/app/h", Type: tar.TypeLink, Link: "../../../etc/passwd"}},
		"whiteout of a parent": {{Name: "opt/app/x/.wh..."}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := ocitest.New(t, "team/app")
			img := r.Push(t, oci.MediaOCILayerGzip, env(), ocitest.Layer(t, l...))
			dest, _, err := pull(t, r, ref(t, r, img.Index))
			if err == nil {
				t.Fatal("pull of an escaping layer worked")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(dest), "escape")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a file landed outside the target: %v", err)
			}
		})
	}
}

func TestPullFailsAStalledLayer(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "team/app")
	img := r.Push(t, oci.MediaOCILayerGzip, env(), ocitest.Layer(t, ocitest.Entry{Name: fileF, Body: strings.Repeat("x", 1<<16)}))
	r.Stall(img.Layers[0])
	_, err := oci.Pull(context.Background(), ref(t, r, img.Index), t.TempDir(),
		oci.Options{HTTP: r.Client(), Arch: amd64, Filter: appFilter(), IdleTimeout: 200 * time.Millisecond})
	if !errors.Is(err, oci.ErrIdle) {
		t.Fatalf("err = %v, want ErrIdle", err)
	}
}

func TestPullSkipsLinksOutOfTheFilterAndOpaqueSymlinks(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "team/app")
	img := r.Push(t, oci.MediaOCILayerGzip, env(),
		ocitest.Layer(t,
			ocitest.Entry{Name: "usr/bin/real", Body: "real"},
			ocitest.Entry{Name: "usr/local/bin/link", Type: tar.TypeLink, Link: "usr/bin/real"},
			ocitest.Entry{Name: "opt/app/b/old", Body: "old"},
			ocitest.Entry{Name: "opt/app/a", Type: tar.TypeSymlink, Link: "b"},
		),
		ocitest.Layer(t,
			ocitest.Entry{Name: "opt/app/b/y", Body: "y"},
			ocitest.Entry{Name: "opt/app/a/.wh..wh..opq"},
		))
	dest, _, err := pull(t, r, ref(t, r, img.Index))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "usr/local/bin/link")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the link to a left-out file exists: %v", err)
	}
	for _, f := range []string{"opt/app/b/old", "opt/app/b/y"} {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestPullRefusesUnsupportedLayers(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "team/app")
	img := r.Push(t, "application/vnd.oci.image.layer.v1.tar+zstd", env(), []byte("zstd"))
	if _, _, err := pull(t, r, ref(t, r, img.Index)); !errors.Is(err, oci.ErrMediaType) {
		t.Fatalf("err = %v, want ErrMediaType", err)
	}
}

func TestPullPlatformMissing(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "team/app")
	img := r.Push(t, oci.MediaOCILayerGzip, env(), ocitest.Layer(t, ocitest.Entry{Name: fileF, Body: "x"}))
	_, err := oci.Pull(context.Background(), ref(t, r, img.Index), t.TempDir(),
		oci.Options{HTTP: r.Client(), Arch: "riscv64", Filter: appFilter()})
	if !errors.Is(err, oci.ErrPlatform) {
		t.Fatalf("err = %v, want ErrPlatform", err)
	}
}

func TestPullSingleManifest(t *testing.T) {
	t.Parallel()
	r := ocitest.New(t, "team/app")
	img := r.Push(t, oci.MediaOCILayerGzip, env(), ocitest.Layer(t, ocitest.Entry{Name: fileF, Body: "x"}))
	dest, _, err := pull(t, r, ref(t, r, img.Manifest))
	if err != nil {
		t.Fatal(err)
	}
	if s := readFile(t, filepath.Join(dest, fileF)); s != "x" {
		t.Errorf("f = %q", s)
	}
}

func TestPullFallsBackToUpstream(t *testing.T) {
	t.Parallel()
	up := ocitest.New(t, "team/app")
	img := up.Push(t, oci.MediaOCILayerGzip, env(), ocitest.Layer(t, ocitest.Entry{Name: fileF, Body: "x"}))
	mirror := ocitest.New(t, "example.com/team/app")
	mirror.RequireBasic()
	mref, err := oci.ParseReference(mirror.Domain() + "/example.com/team/app:v1@" + img.Index)
	if err != nil {
		t.Fatal(err)
	}
	base, ok := up.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("the test server's client has no *http.Transport")
	}
	tr := base.Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "example.com:443" {
			addr = up.Domain()
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	dest := t.TempDir()
	got, err := oci.Pull(context.Background(), mref, dest, oci.Options{HTTP: &http.Client{Transport: tr}, Arch: amd64, Filter: appFilter(),
		Upstreams: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Source.Domain != "example.com" || got.Source.Repository != "team/app" {
		t.Errorf("source = %+v", got.Source)
	}
	if s := readFile(t, filepath.Join(dest, fileF)); s != "x" {
		t.Errorf("f = %q", s)
	}
	if mirror.Requests("/v2/") == 0 {
		t.Error("the mirror was not tried first")
	}
}

func TestParseReference(t *testing.T) {
	t.Parallel()
	const d = "sha256:276d44685c65c0cb2a25f190b63c65c8906f9dc76486fb1c05ae71b5826bda08"
	for in, want := range map[string]oci.Reference{
		"registry.example.com/docker.io/nousresearch/hermes-agent:v0.23.1@" + d: {
			Domain: "registry.example.com", Repository: "docker.io/nousresearch/hermes-agent", Tag: "v0.23.1", Digest: d},
		"nousresearch/hermes-agent@" + d:          {Domain: oci.DockerHub, Repository: "nousresearch/hermes-agent", Digest: d},
		"alpine:3@" + d:                           {Domain: oci.DockerHub, Repository: libAlpine, Tag: "3", Digest: d},
		"localhost:5000/a/b:0.23.1-patch.1@" + d:  {Domain: "localhost:5000", Repository: "a/b", Tag: "0.23.1-patch.1", Digest: d},
		"index.docker.io/library/alpine@" + d:     {Domain: oci.DockerHub, Repository: libAlpine, Digest: d},
		"ghcr.io/example/hermes:v0.23.1-fix@" + d: {Domain: "ghcr.io", Repository: "example/hermes", Tag: "v0.23.1-fix", Digest: d},
	} {
		got, err := oci.ParseReference(in)
		if err != nil || got != want {
			t.Errorf("ParseReference(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"nousresearch/hermes-agent:v0.23.1", "a@sha256:abc", "A/B@" + d, "a/../b@" + d, "a:b:c@" + d, "", "@" + d,
		"a@sha512:" + strings.Repeat("0", 128),
	} {
		if _, err := oci.ParseReference(bad); err == nil {
			t.Errorf("ParseReference(%q) worked", bad)
		}
	}
	if _, err := oci.ParseReference("a/b:v1"); !errors.Is(err, oci.ErrNoDigest) {
		t.Errorf("no digest: %v", err)
	}
}

func TestUpstream(t *testing.T) {
	t.Parallel()
	const d = "sha256:276d44685c65c0cb2a25f190b63c65c8906f9dc76486fb1c05ae71b5826bda08"
	ref, _ := oci.ParseReference("registry.example.com/docker.io/nousresearch/hermes-agent:v0.23.1@" + d)
	up, ok := ref.Upstream(oci.DefaultUpstreams)
	if !ok || up.String() != "docker.io/nousresearch/hermes-agent:v0.23.1@"+d {
		t.Errorf("upstream = %v, %v", up, ok)
	}
	ref, _ = oci.ParseReference("registry.example.com/docker.io/alpine@" + d)
	if up, ok := ref.Upstream(oci.DefaultUpstreams); !ok || up.Repository != libAlpine {
		t.Errorf("upstream = %v, %v", up, ok)
	}
	for _, s := range []string{"registry.example.com/team/app@" + d, "nousresearch/hermes-agent@" + d,
		"registry.example.com/my.group/app@" + d} {
		ref, _ := oci.ParseReference(s)
		if up, ok := ref.Upstream(oci.DefaultUpstreams); ok {
			t.Errorf("%s: upstream %v", s, up)
		}
	}
}

func TestFilter(t *testing.T) {
	t.Parallel()
	f := oci.Filter{Include: []string{"opt/hermes", "usr/local/bin"}, Exclude: []string{"opt/hermes/tools/chromium-*"}}
	for name, want := range map[string]bool{
		"opt/hermes": true, "opt/hermes/x/y": true, "usr/local/bin/node": true, "opt": false, "opt/hermesx": false,
		"opt/hermes/tools/chromium-1208/chrome": false, "opt/hermes/tools/node-26/bin/node": true, "etc/passwd": false,
	} {
		if got := f.Keep(name); got != want {
			t.Errorf("Keep(%q) = %v", name, got)
		}
	}
}
