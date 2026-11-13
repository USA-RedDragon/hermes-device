package web_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
)

func setupDevices(t *testing.T, revoked *[]string) *hubEnv {
	t.Helper()
	return setupWith(t, func(cfg *config.Config, opts *web.Options, st *store.Store) {
		cfg.Backends[0].Devices = true
		opts.Devices = st
		opts.OnRevoke = func(id string) { *revoked = append(*revoked, id) }
		on := true
		opts.Sandboxes = func(_ context.Context, user string) []hub.Sandbox {
			if user != userAlice {
				return nil
			}
			return []hub.Sandbox{{ID: "sandbox-alice", Name: "Sandbox", BackendID: "pc", Online: &on}}
		}
	})
}

func form(t *testing.T, c *http.Client, u, origin string, v url.Values) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, u, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestPairPage(t *testing.T) {
	t.Parallel()
	var revoked []string
	e := setupDevices(t, &revoked)
	ctx := context.Background()
	c := client(t)
	resp, body := get(t, c, e.url+"/device/pair")
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/device/pair" || !strings.Contains(body, `name="code"`) {
		t.Fatalf("pair page = %d %s", resp.StatusCode, resp.Request.URL)
	}
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)[1]
	code, user, err := e.store.StartPairing(ctx, hub.PairingRequest{Name: "desktop", Hostname: "desktop.example.com",
		OS: "linux", Arch: "amd64", ClientVersion: "v0.1.0", Addr: "192.0.2.7"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	res := pairApprove(t, e, c, csrf, code, user)
	pairRejects(t, e, c, csrf)
	pairRevoke(t, e, c, res, &revoked)
}

func pairApprove(t *testing.T, e *hubEnv, c *http.Client, csrf, code, user string) hub.PairPoll {
	t.Helper()
	ctx := context.Background()
	if resp, _ := form(t, c, e.url+"/device/pair", "https://evil.example.com", url.Values{fieldCSRF: {csrf}, fieldCode: {user}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin = %d", resp.StatusCode)
	}
	if resp, _ := form(t, c, e.url+"/device/pair", "", url.Values{fieldCSRF: {"x"}, fieldCode: {user}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad csrf = %d", resp.StatusCode)
	}
	resp, body := form(t, c, e.url+"/device/pair", e.url, url.Values{fieldCSRF: {csrf}, fieldCode: {"BBBB-BBBB"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "wrong or has expired") {
		t.Fatalf("wrong code = %d %s", resp.StatusCode, body)
	}
	resp, body = form(t, c, e.url+"/device/pair", e.url, url.Values{fieldCSRF: {csrf}, fieldCode: {strings.ToLower(user)}, fieldAction: {"lookup"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "192.0.2.7") || !strings.Contains(body, "desktop.example.com") ||
		!strings.Contains(body, "Connect this computer") {
		t.Fatalf("confirm = %d %s", resp.StatusCode, body)
	}
	resp, body = form(t, c, e.url+"/device/pair", e.url, url.Values{fieldCSRF: {csrf}, fieldCode: {user}, fieldAction: {actionApprove},
		fieldName: {laptopName}, fieldBackend: {"pc"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "is now connected") {
		t.Fatalf("approve = %d %s", resp.StatusCode, body)
	}
	res, err := e.store.PollPairing(ctx, code, time.Second)
	if err != nil || res.Status != hub.PairApproved || res.Device.Name != laptopName || res.Device.BackendID != "pc" || res.Device.User != userAlice {
		t.Fatalf("poll = %+v %v", res, err)
	}
	return res
}

func pairRejects(t *testing.T, e *hubEnv, c *http.Client, csrf string) {
	t.Helper()
	ctx := context.Background()
	_, user2, _ := e.store.StartPairing(ctx, hub.PairingRequest{Name: laptopName}, time.Minute)
	resp, body := form(t, c, e.url+"/device/pair", e.url, url.Values{fieldCSRF: {csrf}, fieldCode: {user2}, fieldAction: {actionApprove},
		fieldName: {laptopName}, fieldBackend: {"pc"}})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "already have a computer with that name") {
		t.Fatalf("same name = %d %s", resp.StatusCode, body)
	}
	if resp, _ := form(t, c, e.url+"/device/pair", e.url, url.Values{fieldCSRF: {csrf}, fieldCode: {user2}, fieldAction: {actionApprove},
		fieldName: {"other"}, fieldBackend: {backendHome}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("backend without devices = %d", resp.StatusCode)
	}
	resp, body = form(t, c, e.url+"/device/pair", e.url, url.Values{fieldCSRF: {csrf}, fieldCode: {user2}, fieldAction: {"deny"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "was not connected") {
		t.Fatalf("deny = %d %s", resp.StatusCode, body)
	}
}

func pairRevoke(t *testing.T, e *hubEnv, c *http.Client, res hub.PairPoll, revoked *[]string) {
	t.Helper()
	ctx := context.Background()
	m := login(t, e, c)
	opened := time.Unix(1_800_000_000, 0)
	if err := e.store.ExecLegOpened(ctx, res.Device.ID, "leg-1"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.ExecLegClosed(ctx, res.Device.ID, "leg-1", hub.LegClose{Code: 4426, Reason: "upgrade them together",
		By: hub.CloseByBackend, At: opened.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	resp, body := get(t, c, e.url+"/api/devices")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"name":"work laptop"`) || !strings.Contains(body, `"paired":true`) {
		t.Fatalf("devices = %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"last_close":{"code":4426,"reason":"upgrade them together","by":"backend","at":"`) {
		t.Fatalf("last close not listed: %s", body)
	}
	if !strings.Contains(body, `"kind":"sandbox"`) || !strings.Contains(body, `"online":true`) || !strings.Contains(body, `"kind":"paired"`) {
		t.Fatalf("sandbox not listed: %s", body)
	}
	if resp, _ := post(t, c, e.url+"/api/devices/"+res.Device.ID+"/rename", m.CSRF, e.url, map[string]string{fieldName: " "}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("rename to blank = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, e.url+"/api/devices/sandbox-alice/rename", m.CSRF, e.url, map[string]string{fieldName: "x"}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("rename sandbox = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, e.url+"/api/devices/"+res.Device.ID+"/rename", m.CSRF, e.url, map[string]string{fieldName: " desk "}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("rename = %d", resp.StatusCode)
	}
	if _, body := get(t, c, e.url+"/api/devices"); !strings.Contains(body, `"name":"desk"`) {
		t.Fatalf("renamed device not listed: %s", body)
	}
	if resp, _ := post(t, c, e.url+"/api/devices/sandbox-alice/revoke", m.CSRF, e.url, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke sandbox = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, e.url+"/api/devices/"+res.Device.ID+"/revoke", "", e.url, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("revoke without csrf = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, e.url+"/api/devices/"+res.Device.ID+"/revoke", m.CSRF, e.url, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d", resp.StatusCode)
	}
	if len(*revoked) != 1 || (*revoked)[0] != res.Device.ID {
		t.Fatalf("OnRevoke got %v", *revoked)
	}
	if resp, _ := post(t, c, e.url+"/api/devices/"+res.Device.ID+"/revoke", m.CSRF, e.url, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second revoke = %d", resp.StatusCode)
	}
	if _, ok, _ := e.store.DeviceByToken(ctx, res.Token); ok {
		t.Fatal("revoked token still works")
	}
}

func TestPairCodeLimit(t *testing.T) {
	t.Parallel()
	var revoked []string
	e := setupDevices(t, &revoked)
	c := client(t)
	_, body := get(t, c, e.url+"/device/pair")
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)[1]
	for range 10 {
		form(t, c, e.url+"/device/pair", e.url, url.Values{fieldCSRF: {csrf}, fieldCode: {"BBBB-BBBB"}})
	}
	_, user, _ := e.store.StartPairing(context.Background(), hub.PairingRequest{Name: "x"}, time.Minute)
	resp, body := form(t, c, e.url+"/device/pair", e.url, url.Values{fieldCSRF: {csrf}, fieldCode: {user}})
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "Too many wrong codes") {
		t.Fatalf("locked = %d %s", resp.StatusCode, body)
	}
}

func TestPairPageOff(t *testing.T) {
	t.Parallel()
	e := setup(t)
	if resp, _ := get(t, client(t), e.url+"/device/pair"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pair page with devices off = %d", resp.StatusCode)
	}
}
