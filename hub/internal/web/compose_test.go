package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const (
	pathS1     = "/api/sessions/pc/s1"
	keyText    = "text"
	keyStatus  = "status"
	keyModel   = "model"
	statusSent = "sent"
)

func putJSON(t *testing.T, c *http.Client, u, csrf string, body any) int {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPut, u, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

type listedJSON struct {
	ID        string    `json:"id"`
	Pinned    bool      `json:"pinned"`
	Unread    bool      `json:"unread"`
	Needs     string    `json:"needs"`
	UpdatedAt time.Time `json:"updated_at"`
}

func listed(t *testing.T, e *hubEnv, c *http.Client) map[string]listedJSON {
	t.Helper()
	var ss []listedJSON
	if s := apiJSON(t, c, http.MethodGet, e.url+"/api/sessions", "", nil, &ss); s != http.StatusOK {
		t.Fatalf("sessions = %d", s)
	}
	out := map[string]listedJSON{}
	for _, x := range ss {
		out[x.ID] = x
	}
	return out
}

func TestDraftsAPI(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	m := login(t, e, c)
	u := e.url + pathS1 + "/draft"
	var d struct {
		Text      string    `json:"text"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if s := apiJSON(t, c, http.MethodGet, u, "", nil, &d); s != http.StatusOK || d.Text != "" {
		t.Fatalf("empty draft = %d %+v", s, d)
	}
	if s := putJSON(t, c, u, m.CSRF, map[string]string{keyText: "half a thought"}); s != http.StatusNoContent {
		t.Fatalf("put = %d", s)
	}
	if s := apiJSON(t, c, http.MethodGet, u, "", nil, &d); s != http.StatusOK || d.Text != "half a thought" || d.UpdatedAt.IsZero() {
		t.Fatalf("draft = %d %+v", s, d)
	}
	if s := putJSON(t, c, u, m.CSRF, map[string]string{keyText: strings.Repeat("x", 64<<10+1)}); s != http.StatusRequestEntityTooLarge {
		t.Fatalf("big draft = %d", s)
	}
	if s := putJSON(t, c, u, "", map[string]string{keyText: "x"}); s != http.StatusForbidden {
		t.Fatalf("put without CSRF = %d", s)
	}
	if s := putJSON(t, c, u, m.CSRF, map[string]string{keyText: ""}); s != http.StatusNoContent {
		t.Fatalf("clear = %d", s)
	}
	if s := apiJSON(t, c, http.MethodGet, u, "", nil, &d); s != http.StatusOK || d.Text != "" {
		t.Fatalf("cleared draft = %d %+v", s, d)
	}
	e.is.as("sub-bob", userBob, "family")
	bob := client(t)
	bm := login(t, e, bob)
	if s := putJSON(t, bob, u, bm.CSRF, map[string]string{keyText: "x"}); s != http.StatusForbidden {
		t.Fatalf("bob's draft on alice's backend = %d", s)
	}
}

func TestPinsAndReadState(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.gw.AddSession(&gatewaytest.Session{ID: "s-new", Title: "New", Live: true, StartedAt: float64(time.Now().Add(time.Hour).Unix())})
	c := client(t)
	m := login(t, e, c)
	got := listed(t, e, c)
	if got["s1"].Unread || got["s1"].Pinned || !got["s-new"].Unread {
		t.Fatalf("first list = %+v", got)
	}
	if s := apiJSON(t, c, http.MethodPost, e.url+pathS1+"/pin", m.CSRF, map[string]bool{"pinned": true}, nil); s != http.StatusNoContent {
		t.Fatalf("pin = %d", s)
	}
	if s := apiJSON(t, c, http.MethodPost, e.url+pathS1+"/read", m.CSRF, map[string]bool{"unread": true}, nil); s != http.StatusNoContent {
		t.Fatalf("mark unread = %d", s)
	}
	got = listed(t, e, c)
	if !got["s1"].Pinned || !got["s1"].Unread {
		t.Fatalf("after pin and unread = %+v", got["s1"])
	}
	at := got["s-new"].UpdatedAt
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/sessions/pc/s-new/read", m.CSRF, map[string]any{"at": at}, nil); s != http.StatusNoContent {
		t.Fatalf("read = %d", s)
	}
	if s := apiJSON(t, c, http.MethodPost, e.url+pathS1+"/read", m.CSRF, map[string]any{"at": time.Now()}, nil); s != http.StatusNoContent {
		t.Fatalf("read s1 = %d", s)
	}
	got = listed(t, e, c)
	if got["s1"].Unread || !got["s1"].Pinned {
		t.Fatalf("s1 after read = %+v", got["s1"])
	}
	if !got["s-new"].Unread {
		t.Fatalf("s-new after a read in the future = %+v, want unread: the hub stores a read time no later than now", got["s-new"])
	}
}

func TestPromptSteerAndCommands(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	m := login(t, e, c)
	e.gw.QueueWhileRunning = true
	e.gw.StartElsewhere("s1", "tui-1", "work", false, true)
	var out map[string]any
	if s := apiJSON(t, c, http.MethodPost, e.url+pathS1+"/prompt", m.CSRF, map[string]any{keyText: "and the docs", "steer": true}, &out); s != http.StatusAccepted ||
		out["steered"] != true || out[keyStatus] != statusSent {
		t.Fatalf("steer = %d %v", s, out)
	}
	out = nil
	if s := apiJSON(t, c, http.MethodPost, e.url+pathS1+"/prompt", m.CSRF, map[string]any{keyText: "/usage"}, &out); s != http.StatusAccepted ||
		out["status"] != "command" || out["output"] != "Tokens: 42" || out["steered"] != false {
		t.Fatalf("command = %d %v", s, out)
	}
	out = nil
	if s := apiJSON(t, c, http.MethodPost, e.url+pathS1+"/prompt", m.CSRF, map[string]any{keyText: "/export"}, &out); s != http.StatusBadRequest ||
		!strings.Contains(errText(out), "does not run this command") || !strings.Contains(errText(out), "hermes profile export") {
		t.Fatalf("refused command = %d %v", s, out)
	}
	var cmds []hub.Command
	if s := apiJSON(t, c, http.MethodGet, e.url+pathS1+"/commands", "", nil, &cmds); s != http.StatusOK ||
		!slices.ContainsFunc(cmds, func(c hub.Command) bool { return c.Name == "/title" && c.Args == "[name]" }) ||
		slices.ContainsFunc(cmds, func(c hub.Command) bool { return c.Name == "/model" }) {
		t.Fatalf("commands = %d %+v", s, cmds)
	}
}

func TestRewindBranchAndTranscriptAPI(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.gw.AddSession(&gatewaytest.Session{ID: "s2", Title: "Two", Live: true, StartedAt: 50, Messages: []map[string]any{
		{kRole: roleUser, keyText: "first"}, {kRole: roleAssistant, keyText: "one"}}})
	c := client(t)
	m := login(t, e, c)
	var none map[string]any
	if s := apiJSON(t, c, http.MethodPost, e.url+pathS1+"/rewind", m.CSRF, map[string]any{"retry": true}, &none); s != http.StatusConflict ||
		!strings.Contains(errText(none), "no message of yours") {
		t.Fatalf("rewind with no message = %d %v", s, none)
	}
	var tr struct {
		Title    string            `json:"title"`
		Messages []json.RawMessage `json:"messages"`
	}
	if s := apiJSON(t, c, http.MethodGet, e.url+"/api/sessions/pc/s2/transcript", "", nil, &tr); s != http.StatusOK || tr.Title != "Two" ||
		len(tr.Messages) != 2 || !strings.Contains(string(tr.Messages[0]), `"row_id":1`) {
		t.Fatalf("transcript = %d %+v", s, tr)
	}
	var br map[string]any
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/sessions/pc/s2/branch", m.CSRF, map[string]any{"row_id": 1, "title": "Fork"}, &br); s != http.StatusCreated ||
		br["backend_id"] != "pc" || br["session_id"] == "" {
		t.Fatalf("branch = %d %v", s, br)
	}
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/sessions/pc/s2/branch", m.CSRF, map[string]any{"row_id": 0}, nil); s != http.StatusBadRequest {
		t.Fatalf("branch at row 0 = %d", s)
	}
	var rw map[string]any
	if s := apiJSON(t, c, http.MethodPost, e.url+"/api/sessions/pc/s2/rewind", m.CSRF, map[string]any{keyText: "first, again"}, &rw); s != http.StatusAccepted ||
		rw["removed"] != float64(2) || rw["id"] == "" || rw[keyStatus] != statusSent {
		t.Fatalf("rewind = %d %v", s, rw)
	}
	e.gw.RequireConfirm("big")
	deadline := time.Now().Add(5 * time.Second)
	for {
		rw = nil
		s := apiJSON(t, c, http.MethodPost, e.url+"/api/sessions/pc/s2/rewind", m.CSRF, map[string]any{"retry": true, keyModel: "big"}, &rw)
		if s == http.StatusOK && rw["status"] == hub.ModelConfirm && rw["confirm"] == "tok-big" {
			break
		}
		if s != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("rewind with a model to confirm = %d %v", s, rw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBranchFollowsItsParentsPlace(t *testing.T) {
	t.Parallel()
	e := setupComputers(t)
	c := client(t)
	m := login(t, e.hubEnv, c)
	started, plain := startChats(t, e, c, m.CSRF)
	branch := func(id string) (int, string, map[string]any) {
		t.Helper()
		before := e.gw.SessionIDs()
		var out map[string]any
		s := apiJSON(t, c, http.MethodPost, e.url+"/api/sessions/pc/"+id+"/branch", m.CSRF, map[string]any{}, &out)
		child := ""
		for _, x := range e.gw.SessionIDs() {
			if !slices.Contains(before, x) {
				child = x
			}
		}
		if sid, _ := out["session_id"].(string); sid != "" {
			child = sid
		}
		return s, child, out
	}
	place := func(id string) hub.Place {
		t.Helper()
		var p hub.Place
		if s := apiJSON(t, c, http.MethodGet, e.url+"/api/sessions/pc/"+id+"/place", "", nil, &p); s != http.StatusOK {
			t.Fatalf("place of %s = %d", id, s)
		}
		return p
	}
	s, child, _ := branch(started)
	if p := place(child); s != http.StatusCreated || p.Kind != hub.PlaceComputer || p.DeviceID != e.id || p.Dir != srcAlice || !p.Inherited {
		t.Fatalf("branch of a computer chat = %d, place %+v", s, p)
	}
	s, child, _ = branch(plain)
	if p := place(child); s != http.StatusCreated || p.Kind != hub.PlaceSandbox {
		t.Fatalf("branch of a sandbox chat = %d, place %+v", s, p)
	}
	e.gw.BranchWithoutParent()
	s, child, out := branch(started)
	if s != http.StatusConflict || !strings.Contains(errText(out), "would not run where this chat runs") {
		t.Fatalf("branch that lost its parent = %d %v", s, out)
	}
	for _, call := range e.gw.Calls() {
		if call.Method == gateway.MethodMutate && call.Params["operation"] == gateway.OpDelete {
			child, _ = call.Params["session_id"].(string)
		}
	}
	if _, ok := e.gw.Session(child); ok || !strings.HasPrefix(child, "branch-") {
		t.Fatalf("the misplaced branch %q was not deleted", child)
	}
	if n := slices.IndexFunc(e.gw.Calls(), func(c gatewaytest.Call) bool {
		return c.Method == gateway.MethodPromptSubmit && c.Params["session_id"] == child
	}); n >= 0 {
		t.Fatal("a prompt reached the misplaced branch")
	}
}
