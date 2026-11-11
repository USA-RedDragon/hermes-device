package firefoxtest_test

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/browser/bidi"
	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
	"github.com/USA-RedDragon/hermes-device/protocol"
)

const (
	e2eDevice = "d_e2e"
	e2eToken  = protocol.BrowserTokenPrefix + "e2e-token"
	e2eUser   = "alice"
)

// fakeHub plays hermes-hub's browser sign-in: the consent page, the code
// exchange with PKCE, and revocation.
type fakeHub struct {
	t         *testing.T
	mu        sync.Mutex
	challenge string
	code      string
	consents  int
	revoked   []string
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/browser/authorize":
		q := r.URL.Query()
		if q.Get("client_id") != protocol.BrowserExtensionID || q.Get("redirect_uri") != protocol.BrowserRedirectURL(protocol.BrowserExtensionID) ||
			q.Get("code_challenge_method") != "S256" || q.Get("device_id") != e2eDevice || q.Get("state") == "" ||
			len(q.Get("instance")) < 16 || !strings.HasPrefix(q.Get("browser"), "Firefox") {
			h.t.Errorf("authorize request: %v", q)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.consents++
		h.mu.Unlock()
		var fields strings.Builder
		for k, v := range q {
			fmt.Fprintf(&fields, `<input type="hidden" name="%s" value="%s">`, html.EscapeString(k), html.EscapeString(v[0]))
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<title>Allow Firefox</title><form method="post" action="/browser/authorize">%s`+
			`<button id="allow" name="action" value="approve">Allow</button></form>`, fields.String())
	case r.Method == http.MethodPost && r.URL.Path == "/browser/authorize":
		_ = r.ParseForm()
		h.mu.Lock()
		h.challenge, h.code = r.PostForm.Get("code_challenge"), "code-e2e"
		h.mu.Unlock()
		back := protocol.BrowserRedirectURL(protocol.BrowserExtensionID) + "?" +
			url.Values{"code": {"code-e2e"}, "state": {r.PostForm.Get("state")}}.Encode()
		http.Redirect(w, r, back, http.StatusSeeOther)
	case r.Method == http.MethodPost && r.URL.Path == "/api/browser/token":
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		sum := sha256.Sum256([]byte(req["code_verifier"]))
		h.mu.Lock()
		ok := h.code != "" && req["code"] == h.code &&
			subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(h.challenge)) == 1
		h.code = ""
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok || req["device_id"] != e2eDevice || req["grant_type"] != "authorization_code" {
			h.t.Errorf("token request: %v", req)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","message":"bad code"}`))
			return
		}
		answer := map[string]string{"token": e2eToken, "session_id": "b1", "user": e2eUser, "device_id": e2eDevice,
			"device_name": "desktop"}
		if err := json.NewEncoder(w).Encode(answer); err != nil {
			h.t.Error(err)
		}
	case r.Method == http.MethodPost && r.URL.Path == "/api/browser/revoke":
		h.mu.Lock()
		h.revoked = append(h.revoked, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		h.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// signInFlow checks that the extension refuses everything before it is
// signed in, then signs in through the hub's page, and checks that the
// credential reaches the daemon. BiDi cannot open or script extension
// pages, so the daemon starts the flow, as the toolbar menu's button does.
func signInFlow(ctx context.Context, t *testing.T, b *broker.Broker, bc *bidi.Client, hub *fakeHub) {
	t.Helper()
	b.SetAuth(broker.AuthStatus{State: broker.AuthSignedOut, Reason: "Firefox is not signed in to Hermes"})
	if _, err := b.Call(ctx, "tabs_list", map[string]any{}); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("tabs_list before sign-in: err = %v", err)
	}
	if !b.RequestSignIn() {
		t.Fatal("no extension to sign in")
	}
	consent := waitContext(ctx, t, bc, func(u string) bool { return strings.Contains(u, "/browser/authorize?") })
	evalString(ctx, t, bc, consent, `document.getElementById("allow").click(), "ok"`)
	deadline := time.Now().Add(20 * time.Second)
	for b.Token() != e2eToken {
		if time.Now().After(deadline) {
			t.Fatalf("the credential did not reach the daemon; token %q", b.Token())
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("signed in: the daemon holds the credential after %d consent page load(s)", hub.consents)
	b.SetAuth(broker.AuthStatus{State: broker.AuthSignedIn, User: e2eUser})
}

// signOutFlow signs out in the popup and checks that the hub revoked the
// credential and the daemon dropped it.
func signOutFlow(ctx context.Context, t *testing.T, b *broker.Broker, hub *fakeHub) {
	t.Helper()
	if !b.RequestSignOut() {
		t.Fatal("no extension to sign out")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		hub.mu.Lock()
		revoked := strings.Join(hub.revoked, ",")
		hub.mu.Unlock()
		if revoked == e2eToken && b.Token() == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sign-out: revoked %q, daemon token %q", revoked, b.Token())
		}
		time.Sleep(100 * time.Millisecond)
	}
	b.SetAuth(broker.AuthStatus{State: broker.AuthSignedOut, Reason: "Firefox is not signed in to Hermes"})
	if _, err := b.Call(ctx, "tabs_list", map[string]any{}); err == nil || !strings.Contains(err.Error(), "not signed in") {
		t.Errorf("tabs_list after sign-out: err = %v", err)
	}
}

// waitContext returns the first top-level browsing context whose URL match
// accepts, such as the window browser.identity opens.
func waitContext(ctx context.Context, t *testing.T, bc *bidi.Client, match func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		res, err := bc.Call(ctx, "browsingContext.getTree", map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		var tree struct {
			Contexts []struct {
				Context string `json:"context"`
				URL     string `json:"url"`
			} `json:"contexts"`
		}
		if err := json.Unmarshal(res, &tree); err != nil {
			t.Fatal(err)
		}
		for _, c := range tree.Contexts {
			if match(c.URL) {
				return c.Context
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no browsing context with the expected URL")
	return ""
}

// evalString evaluates expr in a browsing context, awaiting a promise, and
// returns its string value.
func evalString(ctx context.Context, t *testing.T, bc *bidi.Client, context, expr string) string {
	t.Helper()
	res, err := bc.Call(ctx, "script.evaluate", map[string]any{
		"expression": expr, "awaitPromise": true, "target": map[string]any{"context": context},
	})
	if err != nil {
		t.Fatalf("evaluate %.60q: %v", expr, err)
	}
	var out struct {
		Type   string `json:"type"`
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != "success" {
		t.Fatalf("evaluate %.60q: %s", expr, out.ExceptionDetails.Text)
	}
	s, _ := out.Result.Value.(string)
	return s
}
