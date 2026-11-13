//go:build unix

package oci_test

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/internal/oci"
	"github.com/USA-RedDragon/talaria/internal/oci/ocitest"
)

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
