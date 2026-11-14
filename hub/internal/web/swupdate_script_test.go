package web_test

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A browser that has the previous release's page, script and service
// worker gets the new release's files with one reload, with nothing of the
// old release mixed in. The old release is the fixture in
// testdata/swupdate (its service worker is the one of hub/v0.26.93 and its
// script is served with a long max-age); the same origin then switches to
// the real hub (a request to /__new). testdata/swupdate/run.js drives
// headless Chrome, once with a reload alone and once with the old worker
// asked for the new files first, as the old page's update check did. It
// skips where node or Chrome is missing.
func TestOneReloadReplacesAnOldClient(t *testing.T) {
	t.Parallel()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	chrome := chromePath(t)
	e := setup(t)
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", "swupdate", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	page, script, worker := read("old.html"), read("oldapp.js"), read("oldsw.js")
	real := *e.front.h.Load()
	file := func(w http.ResponseWriter, typ, cache string, b []byte) {
		w.Header().Set("Content-Type", typ)
		w.Header().Set("Cache-Control", cache)
		_, _ = w.Write(b)
	}
	// The old release serves its page, script and worker; login and the
	// API are the hub's, so the old page has a session as a real client does.
	var old http.Handler
	old = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			file(w, "text/html; charset=utf-8", "no-cache", page)
		case "/static/app.js":
			file(w, "text/javascript; charset=utf-8", "max-age=86400", script)
		case "/sw.js":
			file(w, "text/javascript; charset=utf-8", "no-cache", worker)
		case "/__new":
			e.front.to(real)
		case "/__old":
			e.front.to(old)
		default:
			real.ServeHTTP(w, r)
		}
	})
	for _, prefetch := range []string{"reload", "prefetch"} {
		e.front.to(old)
		prof, err := os.MkdirTemp("", "swupdate-profile-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(prof) })
		out, err := exec.CommandContext(t.Context(), node, filepath.Join("testdata", "swupdate", "run.js"), chrome, e.url, prof, prefetch).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", prefetch, err, out)
		}
		t.Logf("%s: %s", prefetch, out)
	}
}
