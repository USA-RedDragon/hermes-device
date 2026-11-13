package firefoxtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/browser/bidi"
	"github.com/USA-RedDragon/talaria/internal/browser/broker"
)

const keySessions = "sessions"

type pageContext struct {
	Tabs []struct {
		TabID  int    `json:"tab_id"`
		URL    string `json:"url"`
		Active bool   `json:"active"`
	} `json:"tabs"`
	Selection *struct {
		TabID int    `json:"tab_id"`
		Text  string `json:"text"`
	} `json:"selection"`
}

// TestFirefoxPageChat checks the page chat's grant (docs/browser.md
// section 6.2): the page chat lets one hub chat read the user's tabs of
// its window, which are not in the Hermes group, and nothing more.
func TestFirefoxPageChat(t *testing.T) { //nolint:paralleltest // one Firefox at a time
	if os.Getenv("HERMES_BROWSER_E2E") != "1" {
		t.Skip("set HERMES_BROWSER_E2E=1 to drive a throwaway headless Firefox")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	b, bc := startFirefox(ctx, t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := "two"
		if r.URL.Path == "/one" {
			name = "one"
		}
		_, _ = io.WriteString(w, `<!doctype html><title>user page `+name+`</title><p id="p">granted text</p><a href="/x">a link</a>`)
	}))
	defer srv.Close()

	user := openContext(ctx, t, bc, srv.URL+"/one", false)
	openContext(ctx, t, bc, srv.URL+"/two", true)
	page := openContext(ctx, t, bc, "moz-extension://"+extUUID+"/pagechat.html", true)
	evalAwait(ctx, t, bc, user, `(() => { const r = document.createRange(); r.selectNodeContents(document.getElementById("p"));
getSelection().removeAllRanges(); getSelection().addRange(r); return 1; })()`)

	var all pageContext
	mustJSONOf(t, evalAwait(ctx, t, bc, page, `context("window", "s1").then(JSON.stringify)`), &all)
	if len(all.Tabs) != 2 || all.Selection == nil || all.Selection.Text != "granted text" {
		t.Fatalf("window context = %+v", all)
	}
	var one, two int
	for _, tab := range all.Tabs {
		if strings.HasSuffix(tab.URL, "/one") && tab.Active {
			one = tab.TabID
		} else if strings.HasSuffix(tab.URL, "/two") {
			two = tab.TabID
		}
	}
	if one == 0 || two == 0 || all.Selection.TabID != one {
		t.Fatalf("tabs = %+v", all)
	}

	var text textResult
	call(ctx, t, b, "read_text", map[string]any{keyTab: two, keySessions: []string{"child", "s1"}}, &text)
	if !strings.Contains(text.Text, "granted text") {
		t.Errorf("read_text of a granted tab = %+v", text)
	}
	var snap snapResult
	call(ctx, t, b, "snapshot", map[string]any{keyTab: one, keySessions: []string{"s1"}}, &snap)
	ref := refByName(snap.Snapshot, "a link")
	for what, args := range map[string]map[string]any{
		"another session": {keyTab: one, keySessions: []string{"s2"}},
		"no session":      {keyTab: one},
	} {
		if _, err := b.Call(ctx, "read_text", args); !refusedErr(err) {
			t.Errorf("read_text with %s: err = %v", what, err)
		}
	}
	if _, err := b.Call(ctx, "click", map[string]any{keyTab: one, keyRef: ref, keySessions: []string{"s1"}}); !refusedErr(err) {
		t.Errorf("click on a granted tab: err = %v", err)
	}

	var only pageContext
	mustJSONOf(t, evalAwait(ctx, t, bc, page, `context("tab", "s1").then(JSON.stringify)`), &only)
	if len(only.Tabs) != 1 || only.Tabs[0].TabID != one {
		t.Fatalf("tab context = %+v", only)
	}
	if _, err := b.Call(ctx, "read_text", map[string]any{keyTab: two, keySessions: []string{"s1"}}); !refusedErr(err) {
		t.Errorf("read_text of a tab the new grant left out: err = %v", err)
	}
	if out := evalAwait(ctx, t, bc, page, `browser.runtime.sendMessage({type: "page_grant", session: "bad id", tabs: []}).then(() => "ok", (e) => "refused")`); out != "refused" {
		t.Errorf("grant with a bad session id = %s", out)
	}
}

func refusedErr(err error) bool {
	var e *broker.Error
	return err != nil && errors.As(err, &e) && e.Refused()
}

func mustJSONOf(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
}

// openContext opens url in a new tab, in the background or not.
func openContext(ctx context.Context, t *testing.T, bc *bidi.Client, url string, background bool) string {
	t.Helper()
	res, err := bc.Call(ctx, "browsingContext.create", map[string]any{"type": "tab", "background": background})
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Context string `json:"context"`
	}
	if err := json.Unmarshal(res, &c); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.Call(ctx, "browsingContext.navigate", map[string]any{"context": c.Context, "url": url, "wait": "complete"}); err != nil {
		t.Fatalf("open %s: %v", url, err)
	}
	return c.Context
}

func evalAwait(ctx context.Context, t *testing.T, bc *bidi.Client, target, expr string) string {
	t.Helper()
	out, err := bc.Call(ctx, "script.evaluate", map[string]any{keyExpr: expr, keyAwait: true,
		keyTarget: map[string]any{keyContext: target}})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	var r struct {
		Type   string `json:"type"`
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.Type != "success" {
		t.Fatalf("script failed: %s", out)
	}
	if s, ok := r.Result.Value.(string); ok {
		return s
	}
	b, err := json.Marshal(r.Result.Value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
