package web_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/web"
)

const (
	browserInstance = "instance-0123456789ab"
	challenge       = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	authorizePath   = "/browser/authorize"
)

type dropped struct{ device, session string }

func setupBrowser(t *testing.T, drops *[]dropped) *hubEnv {
	t.Helper()
	return setupWith(t, func(cfg *config.Config, opts *web.Options, st *store.Store) {
		cfg.Backends[0].Devices = true
		opts.Devices = st
		opts.OnBrowserRevoke = func(d, s string) { *drops = append(*drops, dropped{d, s}) }
	})
}

// browserClient does not follow the redirect to the extension, which
// Firefox catches before it leaves the browser.
func browserClient(t *testing.T) *http.Client {
	t.Helper()
	c := client(t)
	c.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasSuffix(req.URL.Host, ".extensions.allizom.org") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return c
}

func pairFor(t *testing.T, st *store.Store, user, name string) hub.Device {
	t.Helper()
	ctx := context.Background()
	code, uc, err := st.StartPairing(ctx, hub.PairingRequest{Name: name, Hostname: name + ".example.com"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApprovePairing(ctx, uc, user, "pc", name); err != nil {
		t.Fatal(err)
	}
	res, err := st.PollPairing(ctx, code, time.Second)
	if err != nil || res.Status != hub.PairApproved {
		t.Fatalf("poll = %+v %v", res, err)
	}
	return res.Device
}

func authzQuery(deviceID string) url.Values {
	return url.Values{"response_type": {"code"}, "client_id": {protocol.BrowserExtensionID},
		"redirect_uri": {protocol.BrowserRedirectURL(protocol.BrowserExtensionID)}, "state": {"st-1"},
		"code_challenge": {challenge}, "code_challenge_method": {methodS256}, "device_id": {deviceID},
		"instance": {browserInstance}, "browser": {"Firefox 157.0"}}
}

func TestBrowserConsent(t *testing.T) {
	t.Parallel()
	var drops []dropped
	e := setupBrowser(t, &drops)
	d := pairFor(t, e.store, userAlice, "desktop")
	c := browserClient(t)
	q := authzQuery(d.ID)

	resp, body := get(t, c, e.url+authorizePath+"?"+q.Encode())
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != authorizePath || !strings.Contains(body, "Firefox 157.0") ||
		!strings.Contains(body, "desktop.example.com") || !strings.Contains(body, `value="st-1"`) {
		t.Fatalf("consent after login = %d %s %s", resp.StatusCode, resp.Request.URL, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://") ||
		!strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("consent CSP = %q", csp)
	}
	if codes, _ := e.store.BrowserSessions(context.Background(), userAlice); len(codes) != 0 {
		t.Fatal("the consent page changed something")
	}
	csrf := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)[1]

	approve := url.Values{}
	for k, v := range q {
		approve[k] = v
	}
	approve.Set(fieldCSRF, csrf)
	approve.Set(fieldAction, actionApprove)
	if resp, _ := form(t, c, e.url+authorizePath, "https://evil.example.com", approve); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin approve = %d", resp.StatusCode)
	}
	bad := url.Values{}
	for k, v := range approve {
		bad[k] = v
	}
	bad.Set(fieldCSRF, "x")
	if resp, _ := form(t, c, e.url+authorizePath, e.url, bad); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("approve with a bad CSRF token = %d", resp.StatusCode)
	}
	resp, _ = form(t, c, e.url+authorizePath, e.url, approve)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc == nil || loc.Scheme+"://"+loc.Host+loc.Path != protocol.BrowserRedirectURL(protocol.BrowserExtensionID) ||
		loc.Query().Get("state") != "st-1" || loc.Query().Get(fieldCode) == "" {
		t.Fatalf("approve = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	g, ok, err := e.store.RedeemBrowserCode(context.Background(), loc.Query().Get(fieldCode))
	if !ok || err != nil || g.User != userAlice || g.DeviceID != d.ID || g.Challenge != challenge || g.InstanceID != browserInstance {
		t.Fatalf("grant = %+v %v %v", g, ok, err)
	}

	approve.Set(fieldAction, "deny")
	resp, _ = form(t, c, e.url+authorizePath, e.url, approve)
	loc, _ = url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Query().Get("error") != "access_denied" || loc.Query().Get(fieldCode) != "" {
		t.Fatalf("deny = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	consentRefusals(t, e, c, d.ID)
}

func TestBrowserConsentOtherUser(t *testing.T) {
	t.Parallel()
	var drops []dropped
	e := setupBrowser(t, &drops)
	d := pairFor(t, e.store, userAlice, "desktop")
	c := browserClient(t)
	e.is.as("sub-bob", userBob, "family")
	q := authzQuery(d.ID)
	resp, body := get(t, c, e.url+authorizePath+"?"+q.Encode())
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "connected to Hermes as another account") ||
		strings.Contains(body, userAlice) || strings.Contains(body, `name="csrf"`) {
		t.Fatalf("bob on alice's computer = %d %s", resp.StatusCode, body)
	}
	m := login(t, e, c)
	q.Set(fieldCSRF, m.CSRF)
	q.Set(fieldAction, actionApprove)
	resp, _ = form(t, c, e.url+authorizePath, e.url, q)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" {
		t.Fatalf("bob approves for alice's computer = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestBrowserList(t *testing.T) {
	t.Parallel()
	var drops []dropped
	e := setupBrowser(t, &drops)
	d := pairFor(t, e.store, userAlice, "desktop")
	_, b, err := e.store.CreateBrowserSession(context.Background(), userAlice, d.ID, browserInstance, "Firefox 157.0")
	if err != nil {
		t.Fatal(err)
	}
	c := client(t)
	m := login(t, e, c)
	resp, body := get(t, c, e.url+"/api/devices")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"browsers":[{"id":"`+b.ID+`","browser":"Firefox 157.0"`) {
		t.Fatalf("devices = %s", body)
	}
	u := e.url + "/api/devices/" + d.ID + "/browsers/" + b.ID + "/revoke"
	if resp, _ := post(t, c, u, "", e.url, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("revoke without CSRF = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, u, m.CSRF, e.url, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d", resp.StatusCode)
	}
	if len(drops) != 1 || drops[0] != (dropped{d.ID, b.ID}) {
		t.Fatalf("OnBrowserRevoke got %v", drops)
	}
	if resp, _ := post(t, c, u, m.CSRF, e.url, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second revoke = %d", resp.StatusCode)
	}
	if _, body := get(t, c, e.url+"/api/devices"); !strings.Contains(body, `"browsers":[]`) {
		t.Fatalf("devices after revoke = %s", body)
	}
}

// consentRefusals checks requests the consent page refuses without a
// redirect.
func consentRefusals(t *testing.T, e *hubEnv, c *http.Client, deviceID string) {
	t.Helper()
	for name, change := range map[string][2]string{
		"other redirect": {"redirect_uri", "https://evil.example.com/"},
		"other client":   {"client_id", "evil@example.com"},
	} {
		bq := authzQuery(deviceID)
		bq.Set(change[0], change[1])
		resp, body := get(t, c, e.url+authorizePath+"?"+bq.Encode())
		if resp.StatusCode != http.StatusBadRequest || resp.Request.URL.Host != strings.TrimPrefix(e.url, "http://") ||
			!strings.Contains(body, "does not come from the Hermes Firefox extension") {
			t.Fatalf("%s = %d %s", name, resp.StatusCode, resp.Request.URL)
		}
	}
	bq := authzQuery(deviceID)
	bq.Set("code_challenge", "short")
	if resp, _ := get(t, c, e.url+authorizePath+"?"+bq.Encode()); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad challenge = %d", resp.StatusCode)
	}
	if resp, body := get(t, c, e.url+authorizePath+"?"+authzQuery("d_unknown").Encode()); resp.StatusCode != http.StatusNotFound ||
		!strings.Contains(body, "not connected to this Hermes hub") {
		t.Fatalf("unknown device = %d", resp.StatusCode)
	}
}
