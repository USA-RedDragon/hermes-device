//go:build unix

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
