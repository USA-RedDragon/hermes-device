package web_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

const toolOutBase = "/api/sessions/pc/"

func toolOutURL(e *hubEnv, backend, session, query string) string {
	return e.url + "/api/sessions/" + backend + "/" + session + "/tool-output?" + query
}

// A snapshot cuts the long text of tool rows and says how long it was; the
// endpoint gives the whole text back, for this chat only.
func TestSnapshotCutsToolOutputs(t *testing.T) {
	t.Parallel()
	e := setup(t)
	big := strings.Repeat("line of build output\n", 4000)
	whole := `{"output": ` + mustJSON(t, big) + `, "exit_code": 3, "error": null}`
	small := `{"output": "ok", "exit_code": 0}`
	page := `{"ok": true, "path": "/srv/p.html", "title": "` + strings.Repeat("t", 5000) + `"}`
	call := func(id, name string) map[string]any {
		return map[string]any{"id": id, "function": map[string]any{"name": name, "arguments": "{}"}}
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "cut", Title: "Cut", Live: true, StartedAt: 300, Messages: []map[string]any{
		{kRole: roleUser, kText: "go"},
		{kRole: roleAssistant, kText: "", "tool_calls": []any{call("t1", "terminal"), call("t2", "terminal"), call("t3", "page_publish")}},
		{kRole: "tool", "tool_call_id": "t1", "name": "terminal", kText: whole},
		{kRole: "tool", "tool_call_id": "t2", "name": "terminal", kText: small},
		{kRole: "tool", "tool_call_id": "t3", "name": "page_publish", kText: page},
		{kRole: roleAssistant, kText: "done"},
	}})
	e.gw.AddSession(&gatewaytest.Session{ID: "other", Title: "Other", Live: true, StartedAt: 301, Messages: []map[string]any{
		{kRole: roleUser, kText: "go"},
		{kRole: roleAssistant, kText: "", "tool_calls": []any{call("o1", "terminal")}},
		{kRole: "tool", "tool_call_id": "o1", "name": "terminal", kText: "secret of the other chat"},
	}})
	c := client(t)
	login(t, e, c)
	r, stop := stream(t, c, e.url+toolOutBase+"cut/events", "")
	defer stop()
	evs := readEvents(t, r, "snapshot")
	raw := evs[len(evs)-1].data
	var snap struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	if len(raw) > 20000 {
		t.Fatalf("snapshot is %d bytes, want the outputs cut", len(raw))
	}
	byCall := map[string]map[string]any{}
	for _, m := range snap.Messages {
		if id, _ := m["tool_call_id"].(string); id != "" {
			byCall[id] = m
		}
	}
	cut := byCall["t1"]
	text, _ := cut[kText].(string)
	var inner struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal([]byte(text), &inner); err != nil || inner.ExitCode != 3 || len(inner.Output) >= len(big) {
		t.Fatalf("cut row text = %.200q (%v), want valid JSON with the exit code and a short output", text, err)
	}
	if n, _ := cut["output_size"].(float64); int(n) != len(whole) {
		t.Fatalf("output_size = %v, want %d", cut["output_size"], len(whole))
	}
	if n, _ := cut["output_lines"].(float64); int(n) != strings.Count(whole, "\n")+1 {
		t.Fatalf("output_lines = %v", cut["output_lines"])
	}
	if byCall["t2"][kText] != small || byCall["t2"]["output_size"] != nil {
		t.Fatalf("a short output changed: %v", byCall["t2"])
	}
	if byCall["t3"][kText] != page || byCall["t3"]["output_size"] != nil {
		t.Fatal("a page_publish output was cut: its card needs the path")
	}

	resp, body := get(t, c, toolOutURL(e, "pc", "cut", "call=t1"))
	var full struct {
		Text string `json:"text"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &full) != nil || full.Text != whole {
		t.Fatalf("tool-output = %d, %d bytes of %d", resp.StatusCode, len(full.Text), len(whole))
	}
	if rid, ok := cut["row_id"].(float64); ok {
		if resp, body = get(t, c, toolOutURL(e, "pc", "cut", "row="+url.QueryEscape(jsonNum(rid)))); resp.StatusCode != http.StatusOK || !strings.Contains(body, "line of build output") {
			t.Fatalf("by row = %d", resp.StatusCode)
		}
	}
	// Another chat's row is not found through this chat, and the other
	// backend is refused.
	if resp, body = get(t, c, toolOutURL(e, "pc", "cut", "call=o1")); resp.StatusCode != http.StatusNotFound || strings.Contains(body, "secret") {
		t.Fatalf("other chat's call = %d %s", resp.StatusCode, body)
	}
	if resp, _ = get(t, c, toolOutURL(e, "pc", "cut", "call=t2x")); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown call = %d", resp.StatusCode)
	}
	if resp, _ = get(t, c, toolOutURL(e, "pc", "cut", "")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no name = %d", resp.StatusCode)
	}
	if resp, _ = get(t, c, toolOutURL(e, backendHome, "cut", "call=t1")); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other backend = %d", resp.StatusCode)
	}
	if resp, _ = get(t, client(t), toolOutURL(e, "pc", "cut", "call=t1")); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", resp.StatusCode)
	}
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jsonNum(f float64) string {
	b, _ := json.Marshal(int64(f))
	return string(b)
}
