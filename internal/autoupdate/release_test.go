package autoupdate_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/autoupdate"
)

const (
	tagNext  = "v0.15.0"
	tagNow   = "v0.14.0"
	verNow   = "0.14.0"
	keyETag  = "etag"
	keyTag   = "tag"
	keyTried = "tried_at"
)

// fakeGitHub serves /repos/alice/talaria/releases/latest with an
// ETag, answers 304 to a matching If-None-Match, and counts requests.
type fakeGitHub struct {
	t          *testing.T
	tag        string
	prerelease bool
	// limited answers 403 with X-RateLimit-Reset in that many seconds.
	limited  int
	requests atomic.Int32
	notMod   atomic.Int32
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if r.URL.Path != "/repos/alice/talaria/releases/latest" {
		f.t.Errorf("path %q", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	if f.limited > 0 {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Duration(f.limited)*time.Second).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		return
	}
	etag := `"` + f.tag + `"`
	if r.Header.Get("If-None-Match") == etag {
		f.notMod.Add(1)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", etag)
	_, _ = fmt.Fprintf(w, `{"tag_name":%q,"prerelease":%v,"draft":false}`, f.tag, f.prerelease)
}

func newReleases(t *testing.T, f *fakeGitHub) *autoupdate.Releases {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &autoupdate.Releases{
		APIURL: srv.URL, Owner: "alice", Repo: "talaria", Client: srv.Client(),
		Path: filepath.Join(t.TempDir(), "cache", "release.json"),
	}
}

func TestLatestSharesTheCacheForMaxAge(t *testing.T) {
	t.Parallel()
	f := &fakeGitHub{tag: tagNext}
	daemon := newReleases(t, f)
	cli := *daemon
	if _, err := daemon.Latest(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if tag, err := cli.Latest(t.Context(), 24*time.Hour); err != nil || tag != tagNext {
			t.Fatalf("Latest = %q, %v", tag, err)
		}
	}
	if n := f.requests.Load(); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
	cli.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if _, err := cli.Latest(t.Context(), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := f.requests.Load(); n != 2 {
		t.Errorf("%d requests after a day, want 2", n)
	}
}

func TestOwnCacheOnlyIgnoresAnotherSourcesEntry(t *testing.T) {
	t.Parallel()
	f := &fakeGitHub{tag: tagNext}
	cli := newReleases(t, f)
	cli.Source = "cli"
	daemon := *cli
	daemon.Source, daemon.OwnCacheOnly = "daemon", true
	if _, err := cli.Latest(t.Context(), 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Latest(t.Context(), 24*time.Hour); err != nil || f.requests.Load() != 1 {
		t.Fatalf("the cli did not reuse its own entry: %d requests, %v", f.requests.Load(), err)
	}
	if tag, err := daemon.Latest(t.Context(), time.Hour); err != nil || tag != tagNext {
		t.Fatalf("Latest = %q, %v", tag, err)
	}
	if n := f.requests.Load(); n != 2 {
		t.Fatalf("%d requests after the daemon's first lookup, want 2", n)
	}
	for range 3 {
		if _, err := daemon.Latest(t.Context(), time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.requests.Load(); n != 2 {
		t.Errorf("%d requests, want 2: the daemon reuses its own entry", n)
	}
	if err := os.WriteFile(daemon.Path, []byte(`{"tag":"`+tagNext+`","tried_at":"`+time.Now().Format(time.RFC3339)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.Latest(t.Context(), time.Hour); err != nil || f.requests.Load() != 3 {
		t.Errorf("an entry with no source was trusted: %d requests, %v", f.requests.Load(), err)
	}
}

func TestLatestRefusesPrerelease(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]*fakeGitHub{
		"flag":   {tag: "v0.16.0", prerelease: true},
		"suffix": {tag: "v0.16.0-rc.1"},
		"build":  {tag: "v0.16.0+meta"},
		"no v":   {tag: "0.16.0"},
		"short":  {tag: "v0.16"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newReleases(t, f)
			if tag, err := r.Latest(t.Context(), 0); err == nil {
				t.Fatalf("Latest = %q", tag)
			}
			if tag, _ := r.Cached(); tag != "" {
				t.Errorf("cached %q", tag)
			}
		})
	}
}

func TestLatestRateLimitPauses(t *testing.T) {
	t.Parallel()
	f := &fakeGitHub{tag: tagNext, limited: 600}
	r := newReleases(t, f)
	for range 5 {
		if _, err := r.Latest(t.Context(), 0); !errors.Is(err, autoupdate.ErrRateLimited) {
			t.Fatalf("Latest error %v", err)
		}
	}
	if n := f.requests.Load(); n != 1 {
		t.Errorf("%d requests while rate limited, want 1", n)
	}
	r.Now = func() time.Time { return time.Now().Add(11 * time.Minute) }
	f.limited = 0
	if tag, err := r.Latest(t.Context(), 0); err != nil || tag != tagNext {
		t.Fatalf("after the pause: %q, %v", tag, err)
	}
}

func TestLatestDistrustsTheCache(t *testing.T) {
	t.Parallel()
	f := &fakeGitHub{tag: tagNext}
	r := newReleases(t, f)
	now := time.Now()
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]map[string]any{
		"bad tag":         {keyTag: "v9.9.9-evil", keyETag: `"x"`, keyTried: now, "checked_at": now},
		"future check":    {keyTag: "v9.9.9", keyETag: `"x"`, keyTried: now.Add(48 * time.Hour), "checked_at": now.Add(48 * time.Hour)},
		"endless pause":   {"not_before": now.Add(365 * 24 * time.Hour)},
		"not json":        nil,
		"garbage in etag": {keyTag: tagNext, keyETag: "\x00", keyTried: time.Time{}},
	} {
		data := []byte("{")
		if c != nil {
			var err error
			if data, err = json.Marshal(c); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(r.Path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		tag, err := r.Latest(t.Context(), 24*time.Hour)
		if err != nil || tag != tagNext {
			t.Errorf("%s: Latest = %q, %v", name, tag, err)
		}
	}
}

func TestStableAndNewer(t *testing.T) {
	t.Parallel()
	for tag, want := range map[string]bool{
		"v1.2.3": true, "v1.2.3-rc.1": false, "v1.2.3+b": false, "1.2.3": false, "v1.2": false, "v01.2.3": false, "": false,
	} {
		if Stable := autoupdate.Stable(tag); Stable != want {
			t.Errorf("Stable(%q) = %v", tag, Stable)
		}
	}
	for _, tc := range []struct {
		tag, version string
		want         bool
	}{
		{tagNext, verNow, true},
		{tagNext, tagNow, true},
		{tagNow, verNow, false},
		{"v0.13.0", verNow, false},
		{"v0.15.0-rc.1", verNow, false},
		{tagNext, "dev", false},
		{tagNext, "0.14.0-next", false},
	} {
		if got := autoupdate.Newer(tc.tag, tc.version); got != tc.want {
			t.Errorf("Newer(%q, %q) = %v", tc.tag, tc.version, got)
		}
	}
}
