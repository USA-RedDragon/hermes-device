//go:build unix

package setup_test

import (
	"os"
	"testing"
)

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
