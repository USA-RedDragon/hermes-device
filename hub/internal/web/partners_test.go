package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

func listPartners(t *testing.T, c *http.Client, u string) []hub.AgentPartner {
	t.Helper()
	code, body := send(t, c, http.MethodGet, u, "", nil)
	var ps []hub.AgentPartner
	if code != http.StatusOK || json.Unmarshal([]byte(body), &ps) != nil {
		t.Fatalf("list partners = %d %s", code, body)
	}
	return ps
}

func TestPartnersAPI(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.gw.AddSession(&gatewaytest.Session{ID: "s2", Title: "Second", Live: true, StartedAt: 200})
	e.gw.AddSession(&gatewaytest.Session{ID: "s3", Title: "Other", Live: true, StartedAt: 300})
	c := client(t)
	u := e.url + "/api/backends/pc/partners"
	if code, _ := send(t, c, http.MethodGet, u, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("without login = %d", code)
	}
	m := login(t, e, c)
	if _, body := get(t, c, e.url+"/api/me"); !strings.Contains(body, `"partners":true`) {
		t.Fatalf("me = %s", body)
	}
	if ps := listPartners(t, c, u); len(ps) != 0 {
		t.Fatalf("first list = %+v", ps)
	}
	pair := map[string]string{"a": "s2", "b": "s1"}
	if code, _ := send(t, c, http.MethodPost, u, "", pair); code != http.StatusForbidden {
		t.Fatalf("add without CSRF = %d", code)
	}
	code, body := send(t, c, http.MethodPost, u, m.CSRF, pair)
	if code != http.StatusCreated || !strings.Contains(body, `"a":"s1","b":"s2"`) {
		t.Fatalf("add = %d %s", code, body)
	}
	if code, _ := send(t, c, http.MethodPost, u, m.CSRF, map[string]string{"a": "s1", "b": "s2"}); code != http.StatusCreated {
		t.Fatalf("add twice = %d", code)
	}
	for name, body := range map[string]map[string]string{
		"same chat": {"a": "s1", "b": "s1"}, "empty": {"a": "s1", "b": ""},
	} {
		if code, _ := send(t, c, http.MethodPost, u, m.CSRF, body); code != http.StatusBadRequest {
			t.Errorf("add %s = %d", name, code)
		}
	}
	if code, _ := send(t, c, http.MethodPost, u, m.CSRF, map[string]string{"a": "s1", "b": "nope"}); code != http.StatusNotFound {
		t.Fatalf("add an unknown chat = %d", code)
	}
	if code, _ := send(t, c, http.MethodPost, u, m.CSRF, map[string]string{"a": "s1", "b": "s3"}); code != http.StatusCreated {
		t.Fatalf("add s1 s3 = %d", code)
	}
	if ps := listPartners(t, c, u); len(ps) != 2 {
		t.Fatalf("list = %+v", ps)
	}
	if ps := listPartners(t, c, u+"?session=s2"); len(ps) != 1 || ps[0] != hub.NewAgentPartner("s1", "s2") {
		t.Fatalf("list for s2 = %+v", ps)
	}
	if code, _ := send(t, c, http.MethodDelete, u+"/s1/s2", "", nil); code != http.StatusForbidden {
		t.Fatalf("delete without CSRF = %d", code)
	}
	if code, _ := send(t, c, http.MethodDelete, u+"/s2/s1", m.CSRF, nil); code != http.StatusNoContent {
		t.Fatalf("delete in the other order = %d", code)
	}
	if code, _ := send(t, c, http.MethodDelete, u+"/s2/s1", m.CSRF, nil); code != http.StatusNotFound {
		t.Fatalf("delete twice = %d", code)
	}
	if ps := listPartners(t, c, u); len(ps) != 1 {
		t.Fatalf("list after delete = %+v", ps)
	}
}

func TestPartnersAPIOwnerOnly(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.gw.AddSession(&gatewaytest.Session{ID: "s2", Title: "Second", Live: true, StartedAt: 200})
	c := client(t)
	m := login(t, e, c)
	u := e.url + "/api/backends/pc/partners"
	if code, _ := send(t, c, http.MethodPost, u, m.CSRF, map[string]string{"a": "s1", "b": "s2"}); code != http.StatusCreated {
		t.Fatalf("add = %d", code)
	}
	e.is.as("sub-bob", userBob, "family")
	bob := client(t)
	bm := login(t, e, bob)
	if code, _ := send(t, bob, http.MethodGet, u, "", nil); code != http.StatusForbidden {
		t.Fatalf("bob lists alice's pairs = %d", code)
	}
	if code, _ := send(t, bob, http.MethodPost, u, bm.CSRF, map[string]string{"a": "s1", "b": "s2"}); code != http.StatusForbidden {
		t.Fatalf("bob adds = %d", code)
	}
	if code, _ := send(t, bob, http.MethodDelete, u+"/s1/s2", bm.CSRF, nil); code != http.StatusForbidden {
		t.Fatalf("bob deletes = %d", code)
	}
	// A backend of a group has no single owner.
	if code, _ := send(t, bob, http.MethodGet, e.url+"/api/backends/"+backendHome+"/partners", "", nil); code != http.StatusNotFound {
		t.Fatalf("shared backend = %d", code)
	}
	if ps := listPartners(t, c, u); len(ps) != 1 {
		t.Fatalf("alice's pairs after bob's tries = %+v", ps)
	}
}

func TestPartnersAPIOff(t *testing.T) {
	t.Parallel()
	e := setupWith(t, func(_ *config.Config, opts *web.Options, _ *store.Store) { opts.Partners = nil })
	c := client(t)
	login(t, e, c)
	if _, body := get(t, c, e.url+"/api/me"); !strings.Contains(body, `"partners":false`) {
		t.Fatalf("me = %s", body)
	}
	if code, _ := send(t, c, http.MethodGet, e.url+"/api/backends/pc/partners", "", nil); code != http.StatusNotFound {
		t.Fatalf("partners off = %d", code)
	}
}

func TestAgentMessagesAPI(t *testing.T) {
	t.Parallel()
	e := setup(t)
	e.gw.AddSession(&gatewaytest.Session{ID: "s2", Title: "Second", Live: true, StartedAt: 200})
	c := client(t)
	m := login(t, e, c)
	ctx := context.Background()
	for _, am := range []hub.AgentMessage{
		{ID: "am_0000000000000001", BackendID: "pc", FromSession: "s2", FromRoot: "s2", ToSession: "s1", Hop: 4, CreatedAt: time.Now()},
		{ID: "am_0000000000000002", BackendID: "pc", FromSession: "s9", FromRoot: "s9", ToSession: "s1", Hop: 2, CreatedAt: time.Now()},
	} {
		if err := e.store.AddAgentMessage(ctx, am); err != nil {
			t.Fatal(err)
		}
	}
	if code, _ := send(t, c, http.MethodPost, e.url+"/api/backends/pc/partners", m.CSRF, map[string]string{"a": "s1", "b": "s2"}); code != http.StatusCreated {
		t.Fatalf("add pair = %d", code)
	}
	u := e.url + "/api/backends/pc/agent-messages?session=s1&ids=am_0000000000000001,am_0000000000000002,am_ffffffffffffffff"
	code, body := send(t, c, http.MethodGet, u, "", nil)
	var got map[string]struct {
		FromSession string `json:"from_session"`
		Hop         int    `json:"hop"`
		MaxHop      int    `json:"max_hop"`
		Partner     bool   `json:"partner"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || len(got) != 2 {
		t.Fatalf("agent messages = %d %s", code, body)
	}
	if a := got["am_0000000000000001"]; a.FromSession != "s2" || a.Hop != 4 || a.MaxHop != 30 || !a.Partner {
		t.Errorf("partner message = %+v", a)
	}
	if b := got["am_0000000000000002"]; b.Hop != 2 || b.MaxHop != 6 || b.Partner {
		t.Errorf("other message = %+v", b)
	}
	// Without a session nothing counts as a partner.
	_, body = send(t, c, http.MethodGet, e.url+"/api/backends/pc/agent-messages?ids=am_0000000000000001", "", nil)
	if !strings.Contains(body, `"max_hop":6`) || !strings.Contains(body, `"partner":false`) {
		t.Errorf("no session = %s", body)
	}
	if code, _ := send(t, c, http.MethodGet, e.url+"/api/backends/"+backendHome+"/agent-messages", "", nil); code != http.StatusNotFound {
		t.Errorf("shared backend = %d", code)
	}
	many := strings.Repeat("am_0000000000000001,", 51)
	if code, _ := send(t, c, http.MethodGet, e.url+"/api/backends/pc/agent-messages?ids="+many, "", nil); code != http.StatusBadRequest {
		t.Errorf("too many ids = %d", code)
	}
}
