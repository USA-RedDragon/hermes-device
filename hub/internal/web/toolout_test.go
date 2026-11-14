package web_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

const titleOther = "Other"
const toolOutBase = "/api/sessions/pc/"

func toolOutURL(e *hubEnv, backend, query string) string {
	return e.url + "/api/sessions/" + backend + "/cut/tool-output?" + query
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
		return map[string]any{"id": id, fnKey: map[string]any{fieldName: name, ilArgs: "{}"}}
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "cut", Title: "Cut", Live: true, StartedAt: 300, Messages: []map[string]any{
		{kRole: roleUser, kText: "go"},
		{kRole: roleAssistant, kText: "", kCalls: []any{call("t1", toolTerminal), call("t2", toolTerminal), call("t3", "page_publish")}},
		{kRole: stepTool, kCallID: "t1", fieldName: toolTerminal, kText: whole},
		{kRole: stepTool, kCallID: "t2", fieldName: toolTerminal, kText: small},
		{kRole: stepTool, kCallID: "t3", fieldName: "page_publish", kText: page},
		{kRole: roleAssistant, kText: "finished"},
	}})
	e.gw.AddSession(&gatewaytest.Session{ID: "elsewhere", Title: titleOther, Live: true, StartedAt: 301, Messages: []map[string]any{
		{kRole: roleUser, kText: "go"},
		{kRole: roleAssistant, kText: "", kCalls: []any{call("o1", toolTerminal)}},
		{kRole: stepTool, kCallID: "o1", fieldName: toolTerminal, kText: "secret of the other chat"},
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
		if id, _ := m[kCallID].(string); id != "" {
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

	resp, body := get(t, c, toolOutURL(e, "pc", "call=t1"))
	var full struct {
		Text string `json:"text"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &full) != nil || full.Text != whole {
		t.Fatalf("tool-output = %d, %d bytes of %d", resp.StatusCode, len(full.Text), len(whole))
	}
	if rid, ok := cut["row_id"].(float64); ok {
		if resp, body = get(t, c, toolOutURL(e, "pc", "row="+url.QueryEscape(jsonNum(rid)))); resp.StatusCode != http.StatusOK || !strings.Contains(body, "line of build output") {
			t.Fatalf("by row = %d", resp.StatusCode)
		}
	}
	// Another chat's row is not found through this chat, and the other
	// backend is refused.
	if resp, body = get(t, c, toolOutURL(e, "pc", "call=o1")); resp.StatusCode != http.StatusNotFound || strings.Contains(body, "secret") {
		t.Fatalf("other chat's call = %d %s", resp.StatusCode, body)
	}
	if resp, _ = get(t, c, toolOutURL(e, "pc", "call=t2x")); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown call = %d", resp.StatusCode)
	}
	if resp, _ = get(t, c, toolOutURL(e, "pc", "")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no name = %d", resp.StatusCode)
	}
	if resp, _ = get(t, c, toolOutURL(e, backendHome, "call=t1")); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other backend = %d", resp.StatusCode)
	}
	if resp, _ = get(t, client(t), toolOutURL(e, "pc", "call=t1")); resp.StatusCode != http.StatusUnauthorized {
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
	return strconv.FormatInt(int64(f), 10)
}

// The first rows of a chat are the newest turns up to a byte budget; the
// budget counts the rows as the gateway has them, so cutting the outputs
// must not let more turns into the first rows (the page draws them all
// before it shows anything).
func TestCutOutputsDoNotWidenTheFirstRows(t *testing.T) {
	t.Parallel()
	e := setup(t)
	big := strings.Repeat("line of build output\n", 1500)
	call := func(id string) map[string]any {
		return map[string]any{"id": id, fnKey: map[string]any{fieldName: toolTerminal, ilArgs: "{}"}}
	}
	var msgs []map[string]any
	for i := range 60 {
		id := "w" + strconv.Itoa(i)
		msgs = append(msgs,
			map[string]any{kRole: roleUser, kText: "go " + id},
			map[string]any{kRole: roleAssistant, kText: "", kCalls: []any{call(id)}},
			map[string]any{kRole: stepTool, kCallID: id, fieldName: toolTerminal, kText: `{"output": ` + mustJSON(t, big) + `, "exit_code": 0}`},
			map[string]any{kRole: roleAssistant, kText: "done " + id})
	}
	e.gw.AddSession(&gatewaytest.Session{ID: "wide", Title: "Wide", Live: true, StartedAt: 310, Messages: msgs})
	c := client(t)
	login(t, e, c)
	r, stop := stream(t, c, e.url+toolOutBase+"wide/events", "")
	defer stop()
	evs := readEvents(t, r, "snapshot")
	var snap struct {
		Messages []json.RawMessage `json:"messages"`
		Older    int               `json:"older"`
	}
	if err := json.Unmarshal(evs[len(evs)-1].data, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Older == 0 {
		t.Fatal("the snapshot was not split")
	}
	// 32 KB a tool row: the 96 KB budget holds a dozen rows. Cut
	// first, the 40-row limit would be the one that applies.
	if n := len(snap.Messages); n > 30 {
		t.Fatalf("first rows = %d, want the budget of the whole rows (about 14)", n)
	}
}

// Markdown export reads the transcript, not the cut snapshot: a long tool
// output is in it whole.
func TestTranscriptKeepsWholeToolOutputs(t *testing.T) {
	t.Parallel()
	e := setup(t)
	big := strings.Repeat("line of build output\n", 4000)
	call := map[string]any{"id": "x1", fnKey: map[string]any{fieldName: toolTerminal, ilArgs: "{}"}}
	e.gw.AddSession(&gatewaytest.Session{ID: "export", Title: "Export", Live: true, StartedAt: 320, Messages: []map[string]any{
		{kRole: roleUser, kText: "go"},
		{kRole: roleAssistant, kText: "", kCalls: []any{call}},
		{kRole: stepTool, kCallID: "x1", fieldName: toolTerminal, kText: big},
	}})
	c := client(t)
	login(t, e, c)
	_, body := get(t, c, e.url+"/api/sessions/pc/export/transcript")
	if !strings.Contains(body, strings.Repeat("line of build output\\n", 4000)) {
		t.Fatalf("transcript is %d bytes, want the whole output", len(body))
	}
}
