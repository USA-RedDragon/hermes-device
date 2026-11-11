package autoupdate_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/autoupdate"
)

func TestNoticeDoesNotWaitForTheNetwork(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{"tag_name":"v0.15.0"}`))
	}))
	t.Cleanup(srv.Close)
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	var out bytes.Buffer
	n := &autoupdate.Notice{
		Releases: &autoupdate.Releases{APIURL: srv.URL, Owner: "alice", Repo: "talaria", Client: srv.Client(),
			Path: filepath.Join(t.TempDir(), "release.json")},
		Version: verNow, Out: &out,
	}
	begin := time.Now()
	n.Start(t.Context())
	if d := time.Since(begin); d > 500*time.Millisecond {
		t.Errorf("Start took %s", d)
	}
	begin = time.Now()
	n.Finish()
	if d := time.Since(begin); d > autoupdate.NoticeGrace+time.Second {
		t.Errorf("Finish took %s", d)
	}
	if out.Len() != 0 {
		t.Errorf("printed %q with no answer", out.String())
	}
	unblock()
	n.Finish()
	if _, checked := n.Releases.Cached(); checked.IsZero() {
		t.Error("the background lookup did not finish")
	}
}

func TestNoticeIsCachedForADay(t *testing.T) {
	t.Parallel()
	f := &fakeGitHub{tag: tagNext}
	r := newReleases(t, f)
	run := func(version string, auto bool, now time.Time) string {
		var out bytes.Buffer
		rel := *r
		rel.Now = func() time.Time { return now }
		n := &autoupdate.Notice{Releases: &rel, Version: version, Auto: auto, Out: &out}
		n.Start(t.Context())
		n.Finish()
		return out.String()
	}
	now := time.Now()
	got := run(verNow, true, now)
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "v0.15.0 is available (this is v0.14.0)") || !strings.Contains(got, "installs it by itself") {
		t.Errorf("first run printed %q", got)
	}
	got = run(verNow, false, now.Add(time.Hour))
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "Run talaria update to install it.") {
		t.Errorf("second run printed %q", got)
	}
	if run("0.15.0", true, now.Add(2*time.Hour)) != "" || run("dev", true, now.Add(3*time.Hour)) != "" {
		t.Error("printed a notice without a newer release")
	}
	if n := f.requests.Load(); n != 1 {
		t.Errorf("%d lookups in a day, want 1", n)
	}
	run(verNow, true, now.Add(25*time.Hour))
	if n := f.requests.Load(); n != 2 {
		t.Errorf("%d lookups after a day, want 2", n)
	}
}
