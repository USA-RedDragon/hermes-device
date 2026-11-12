package web_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/web"
)

// send makes a request with a JSON body and returns its status and body.
func send(t *testing.T, c *http.Client, method, u, csrf string, body any) (int, string) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = strings.NewReader(string(b))
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, u, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const (
	promptPlan   = "plan"
	promptCheck  = "check"
	promptReview = "review"
	textPlan     = "Plan it."
)

func listPrompts(t *testing.T, c *http.Client, u string) []hub.SavedPrompt {
	t.Helper()
	code, body := send(t, c, http.MethodGet, u, "", nil)
	var ps []hub.SavedPrompt
	if code != http.StatusOK || json.Unmarshal([]byte(body), &ps) != nil {
		t.Fatalf("list prompts = %d %s", code, body)
	}
	return ps
}

func TestPromptsAPI(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	u := e.url + "/api/prompts"
	if code, _ := send(t, c, http.MethodGet, u, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("without login = %d", code)
	}
	m := login(t, e, c)
	if _, body := get(t, c, e.url+"/api/me"); !strings.Contains(body, `"prompts":true`) {
		t.Fatalf("me = %s", body)
	}
	if ps := listPrompts(t, c, u); len(ps) != 0 {
		t.Fatalf("first list = %+v", ps)
	}
	review := map[string]string{fieldName: promptReview, keyText: "Review this change."}
	if code, _ := send(t, c, http.MethodPost, u, "", review); code != http.StatusForbidden {
		t.Fatalf("add without CSRF = %d", code)
	}
	code, body := send(t, c, http.MethodPost, u, m.CSRF, review)
	var got hub.SavedPrompt
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &got) != nil || got.Name != promptReview || got.UpdatedAt.IsZero() {
		t.Fatalf("add = %d %s", code, body)
	}
	for _, c0 := range []struct {
		body map[string]string
		code int
		want string
	}{
		{map[string]string{fieldName: "Review", keyText: "x"}, http.StatusConflict, "already have a prompt with that name"},
		{map[string]string{fieldName: "two words", keyText: "x"}, http.StatusBadRequest, "only the letters A to Z, digits and dashes"},
		{map[string]string{fieldName: "", keyText: "x"}, http.StatusBadRequest, "Give the prompt a name"},
		{map[string]string{fieldName: "empty", keyText: "  "}, http.StatusBadRequest, "Give the prompt some text"},
		{map[string]string{fieldName: "big", keyText: strings.Repeat("x", hub.MaxPromptText+1)}, http.StatusBadRequest, "longer than 64 KB"},
	} {
		code, body := send(t, c, http.MethodPost, u, m.CSRF, c0.body)
		if code != c0.code || !strings.Contains(body, c0.want) {
			t.Errorf("add %q = %d %s, want %d %q", c0.body[fieldName], code, body, c0.code, c0.want)
		}
	}
	if code, _ := send(t, c, http.MethodPost, u, m.CSRF, map[string]string{fieldName: promptPlan, keyText: textPlan}); code != http.StatusCreated {
		t.Fatalf("add plan = %d", code)
	}
	code, body = send(t, c, http.MethodPut, u+"/review", m.CSRF, map[string]string{fieldName: promptCheck, keyText: "Check this change."})
	if code != http.StatusOK || !strings.Contains(body, `"name":"check"`) {
		t.Fatalf("rename = %d %s", code, body)
	}
	if code, body := send(t, c, http.MethodPut, u+"/check", m.CSRF, map[string]string{fieldName: promptPlan, keyText: "x"}); code != http.StatusConflict {
		t.Fatalf("rename onto plan = %d %s", code, body)
	}
	if code, _ := send(t, c, http.MethodPut, u+"/review", m.CSRF, map[string]string{fieldName: promptReview, keyText: "x"}); code != http.StatusNotFound {
		t.Fatalf("update the old name = %d", code)
	}
	if code, body := send(t, c, http.MethodPut, u+"/check", m.CSRF, map[string]string{fieldName: promptCheck, keyText: ""}); code != http.StatusBadRequest ||
		!strings.Contains(body, "some text") {
		t.Fatalf("update with no text = %d %s", code, body)
	}
	ps := listPrompts(t, c, u)
	if len(ps) != 2 || ps[0].Name != promptCheck || ps[0].Text != "Check this change." || ps[1].Name != promptPlan {
		t.Fatalf("list = %+v", ps)
	}
}

func TestPromptsAPIIsolation(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	u := e.url + "/api/prompts"
	m := login(t, e, c)
	if code, _ := send(t, c, http.MethodPost, u, m.CSRF, map[string]string{fieldName: promptPlan, keyText: textPlan}); code != http.StatusCreated {
		t.Fatalf("add plan = %d", code)
	}

	e.is.as("sub-bob", userBob, "family")
	bob := client(t)
	bm := login(t, e, bob)
	if ps := listPrompts(t, bob, u); len(ps) != 0 {
		t.Fatalf("bob sees alice's prompts: %+v", ps)
	}
	if code, _ := send(t, bob, http.MethodPut, u+"/plan", bm.CSRF, map[string]string{fieldName: promptPlan, keyText: "Bob's."}); code != http.StatusNotFound {
		t.Fatalf("bob updates alice's prompt = %d", code)
	}
	if code, _ := send(t, bob, http.MethodDelete, u+"/plan", bm.CSRF, nil); code != http.StatusNotFound {
		t.Fatalf("bob deletes alice's prompt = %d", code)
	}
	if code, _ := send(t, bob, http.MethodPost, u, bm.CSRF, map[string]string{fieldName: promptPlan, keyText: "Bob's plan."}); code != http.StatusCreated {
		t.Fatalf("bob adds his own plan = %d", code)
	}

	if ps := listPrompts(t, c, u); len(ps) != 1 || ps[0].Text != textPlan {
		t.Fatalf("alice's prompts after bob's = %+v", ps)
	}
	if code, _ := send(t, c, http.MethodDelete, u+"/plan", "", nil); code != http.StatusForbidden {
		t.Fatalf("delete without CSRF = %d", code)
	}
	if code, _ := send(t, c, http.MethodDelete, u+"/PLAN", m.CSRF, nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := send(t, c, http.MethodDelete, u+"/plan", m.CSRF, nil); code != http.StatusNotFound {
		t.Fatalf("delete twice = %d", code)
	}
	if ps := listPrompts(t, bob, u); len(ps) != 1 || ps[0].Text != "Bob's plan." {
		t.Fatalf("bob's prompts after alice's delete = %+v", ps)
	}
}

func TestPromptsAPIOff(t *testing.T) {
	t.Parallel()
	e := setupWith(t, func(_ *config.Config, opts *web.Options, _ *store.Store) { opts.Prompts = nil })
	c := client(t)
	m := login(t, e, c)
	if _, body := get(t, c, e.url+"/api/me"); !strings.Contains(body, `"prompts":false`) {
		t.Fatalf("me = %s", body)
	}
	if code, _ := send(t, c, http.MethodGet, e.url+"/api/prompts", "", nil); code != http.StatusNotFound {
		t.Fatalf("list = %d", code)
	}
	if code, _ := send(t, c, http.MethodPost, e.url+"/api/prompts", m.CSRF, map[string]string{fieldName: "a", keyText: "b"}); code != http.StatusNotFound {
		t.Fatalf("add = %d", code)
	}
}
