package web_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const (
	kTitle   = "title"
	kText    = "text"
	modelBig = "big"
)

func TestChatManagementAPI(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	m := login(t, e, c)
	base := e.url + "/api/sessions/pc/s1/"
	if resp, _ := post(t, c, base+"rename", "", "", map[string]string{kTitle: "x"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("rename without CSRF = %d", resp.StatusCode)
	}
	if resp, body := post(t, c, base+"rename", m.CSRF, e.url, map[string]string{kTitle: "Renamed"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("rename = %d %s", resp.StatusCode, body)
	}
	if s, _ := e.gw.Session("s1"); s.Title != "Renamed" {
		t.Fatalf("title = %q", s.Title)
	}
	if resp, body := post(t, c, base+"rename", m.CSRF, e.url, map[string]string{kTitle: ""}); resp.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body, "characters") {
		t.Fatalf("empty rename = %d %s", resp.StatusCode, body)
	}
	if resp, body := post(t, c, base+"archive", m.CSRF, e.url, map[string]bool{"archived": true}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("archive = %d %s", resp.StatusCode, body)
	}
	_, body := get(t, c, e.url+"/api/sessions")
	var list []struct {
		ID       string `json:"id"`
		Archived bool   `json:"archived"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil || len(list) != 1 || !list[0].Archived {
		t.Fatalf("sessions = %s", body)
	}
	if resp, _ := post(t, c, e.url+"/api/sessions/pc/nope/delete", m.CSRF, e.url, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete unknown = %d", resp.StatusCode)
	}
	if resp, body := post(t, c, base+"delete", m.CSRF, e.url, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete = %d %s", resp.StatusCode, body)
	}
	if _, ok := e.gw.Session("s1"); ok {
		t.Fatal("session not deleted")
	}

	e.is.as("sub-bob", userBob, "family")
	bob := client(t)
	bm := login(t, e, bob)
	if resp, _ := post(t, bob, e.url+"/api/sessions/pc/s2/rename", bm.CSRF, e.url, map[string]string{kTitle: "x"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob rename on alice's backend = %d", resp.StatusCode)
	}
}

func TestDeleteBusyChat(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	m := login(t, e, c)
	e.gw.HoldTurns = true
	if resp, _ := post(t, c, e.url+"/api/sessions/pc/s1/prompt", m.CSRF, e.url, map[string]string{kText: "hold"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("prompt = %d", resp.StatusCode)
	}
	resp, body := post(t, c, e.url+"/api/sessions/pc/s1/delete", m.CSRF, e.url, nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "still working") {
		t.Fatalf("delete busy = %d %s", resp.StatusCode, body)
	}
}

func TestModelAndReasoningAPI(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	m := login(t, e, c)
	base := e.url + "/api/sessions/pc/s1/"
	resp, body := get(t, c, base+"models")
	var models struct {
		Model     string `json:"model"`
		Providers []struct {
			ID     string `json:"id"`
			Models []struct {
				ID        string `json:"id"`
				Reasoning bool   `json:"reasoning"`
			} `json:"models"`
		} `json:"providers"`
		Efforts []string `json:"efforts"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &models) != nil || models.Model != "small" ||
		len(models.Providers) != 1 || len(models.Providers[0].Models) != 2 || len(models.Efforts) != 8 {
		t.Fatalf("models = %d %s", resp.StatusCode, body)
	}
	e.gw.RequireConfirm(modelBig)
	resp, body = post(t, c, base+"model", m.CSRF, e.url, map[string]string{"provider": "local", "model": modelBig})
	var res struct{ Status, Confirm, Message string }
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &res) != nil || res.Status != "confirm" || res.Confirm == "" {
		t.Fatalf("model = %d %s", resp.StatusCode, body)
	}
	resp, body = post(t, c, base+"model", m.CSRF, e.url, map[string]string{"provider": "local", "model": modelBig, "confirm": res.Confirm})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"status":"changed"`) {
		t.Fatalf("confirmed model = %d %s", resp.StatusCode, body)
	}
	if s, _ := e.gw.Session("s1"); s.Model != modelBig {
		t.Fatalf("model = %q", s.Model)
	}
	if resp, body := post(t, c, base+"reasoning", m.CSRF, e.url, map[string]string{"effort": "high"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("reasoning = %d %s", resp.StatusCode, body)
	}
	if s, _ := e.gw.Session("s1"); s.Effort != "high" {
		t.Fatalf("effort = %q", s.Effort)
	}
	if resp, _ := post(t, c, base+"reasoning", m.CSRF, e.url, map[string]string{"effort": "turbo"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad effort = %d", resp.StatusCode)
	}
}
