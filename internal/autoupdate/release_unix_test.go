//go:build unix

package autoupdate_test

import (
	"os"
	"testing"
)

func TestLatestUsesETag(t *testing.T) {
	t.Parallel()
	f := &fakeGitHub{tag: tagNext}
	r := newReleases(t, f)
	for range 3 {
		tag, err := r.Latest(t.Context(), 0)
		if err != nil || tag != tagNext {
			t.Fatalf("Latest = %q, %v", tag, err)
		}
	}
	if f.requests.Load() != 3 || f.notMod.Load() != 2 {
		t.Errorf("%d requests, %d answered 304; want 3 and 2", f.requests.Load(), f.notMod.Load())
	}
	fi, err := os.Stat(r.Path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("cache file %v, %v", fi, err)
	}
}
