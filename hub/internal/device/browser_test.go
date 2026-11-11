package device_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/USA-RedDragon/hermes-hub/internal/device"
	"github.com/USA-RedDragon/hermes-hub/internal/device/devicetest"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/coder/websocket"
)

const (
	instance  = "instance-0123456789ab"
	firefoxUA = "Firefox 157.0"
	keyOK     = "ok"
	keyCode   = "code"
	keyError  = "error"
)

// pluginGets returns the next message the fake plugin got that is not
// a browser_status, unless wantType is browser_status.
func pluginGets(t *testing.T, pc *devicetest.Conn, wantType string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case raw := <-pc.Messages:
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("plugin got %s", raw)
			}
			if m[keyType] == wantType {
				return m
			}
		case <-deadline:
			t.Fatalf("plugin got no %s", wantType)
		}
	}
}

// deviceGets reads the device's next message, which must be of wantType.
func deviceGets(t *testing.T, c *websocket.Conn, wantType string) map[string]any {
	t.Helper()
	m := readJSON(t, c)
	if m[keyType] != wantType {
		t.Fatalf("device got %v, want a %s", m, wantType)
	}
	return m
}

func sendBrowser(t *testing.T, pc *devicetest.Conn, id string) {
	t.Helper()
	if err := pc.Send(context.Background(), map[string]any{keyType: protocol.TypeBrowser, "id": id, "op": "tabs_list", "timeout": 5}); err != nil {
		t.Fatal(err)
	}
}

func signIn(t *testing.T, st *store.Store, user, deviceID string) (string, hub.BrowserSession) {
	t.Helper()
	tok, b, err := st.CreateBrowserSession(context.Background(), user, deviceID, instance, firefoxUA)
	if err != nil {
		t.Fatal(err)
	}
	return tok, b
}

func presentToken(t *testing.T, c *websocket.Conn, token string) map[string]any {
	t.Helper()
	writeJSON(t, c, protocol.BrowserAuth{Type: protocol.TypeBrowserAuth, Token: token})
	return deviceGets(t, c, protocol.TypeBrowserAuthResult)
}

func TestBrowserSignInOnExecLeg(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		c, pc := e.connectExec(e.a, tok)

		sendBrowser(t, pc, "b1")
		res := pluginGets(t, pc, protocol.TypeBrowserResult)
		if res["id"] != "b1" || res[keyCode] != protocol.BrowserNotSignedIn || res[keyOK] != false ||
			res[keyError] != "Firefox is not signed in to Hermes on "+deviceName {
			t.Fatalf("browser before sign-in = %v", res)
		}
		writeJSON(t, c, protocol.BrowserStatus{Type: protocol.TypeBrowserStatus, Connected: true, SignedIn: true, User: "mallory",
			Browser: firefoxUA, ExtensionVersion: "0.5.0"})
		st1 := pluginGets(t, pc, protocol.TypeBrowserStatus)
		if st1["connected"] != false || st1["signed_in"] != false || st1["user"] != nil ||
			st1["reason"] != "Firefox is not signed in to Hermes" || st1["browser"] != firefoxUA || st1["extension_version"] != "0.5.0" {
			t.Fatalf("status before sign-in = %v", st1)
		}

		btok, b := signIn(t, st, userAlice, id)
		r := presentToken(t, c, btok)
		if r[keyOK] != true || r["user"] != userAlice || r["session_id"] != b.ID {
			t.Fatalf("auth result = %v", r)
		}
		st2 := pluginGets(t, pc, protocol.TypeBrowserStatus)
		if st2["connected"] != true || st2["signed_in"] != true || st2["user"] != userAlice || st2["reason"] != nil {
			t.Fatalf("status after sign-in = %v", st2)
		}
		sendBrowser(t, pc, "b2")
		if m := deviceGets(t, c, protocol.TypeBrowser); m["id"] != "b2" {
			t.Fatalf("browser = %v", m)
		}

		forgedDropped(t, c, pc)
		revokedAtNextOp(t, st, c, pc, b.ID)
	})
}

func TestBrowserSignInRefusals(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		_, other := e.pairAs(e.a, "laptop")
		c, _ := e.connectExec(e.a, tok)

		bobTok, _ := signIn(t, st, userBob, id)
		r := presentToken(t, c, bobTok)
		if r[keyCode] != protocol.BrowserAuthWrongUser || !strings.Contains(str(t, r, keyError), "with another account") {
			t.Fatalf("bob's sign-in = %v", r)
		}
		otherTok, _ := signIn(t, st, userAlice, other)
		if r := presentToken(t, c, otherTok); r[keyCode] != protocol.BrowserAuthOtherDevice {
			t.Fatalf("other device's sign-in = %v", r)
		}
		if r := presentToken(t, c, ""); r[keyCode] != protocol.BrowserAuthSignedOut {
			t.Fatalf("no token = %v", r)
		}
		if r := presentToken(t, c, protocol.BrowserTokenPrefix+strings.Repeat("x", 43)); r[keyCode] != protocol.BrowserAuthInvalid {
			t.Fatalf("unknown token = %v", r)
		}
	})
}

func TestBrowserRevokeReachesEveryReplica(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		tok, id := e.pair(e.a)
		c, pc := e.connectExec(e.b, tok)
		btok, _ := signIn(t, st, userAlice, id)
		if r := presentToken(t, c, btok); r[keyOK] != true {
			t.Fatalf("auth = %v", r)
		}
		if s, _ := call(t, http.MethodPost, e.a.url+"/api/browser/revoke", btok, nil); s != http.StatusNoContent {
			t.Fatalf("revoke = %d", s)
		}
		if r := deviceGets(t, c, protocol.TypeBrowserAuthResult); r[keyCode] != protocol.BrowserAuthInvalid {
			t.Fatalf("recheck on the other replica = %v", r)
		}
		sendBrowser(t, pc, "b1")
		if res := pluginGets(t, pc, protocol.TypeBrowserResult); res[keyCode] != protocol.BrowserNotSignedIn {
			t.Fatalf("browser after revoke = %v", res)
		}
		if s, _ := call(t, http.MethodPost, e.a.url+"/api/browser/revoke", btok, nil); s != http.StatusNoContent {
			t.Fatalf("second revoke = %d", s)
		}
		if s, _ := call(t, http.MethodPost, e.a.url+"/api/browser/revoke", "", nil); s != http.StatusUnauthorized {
			t.Fatalf("revoke without a token = %d", s)
		}

		btok2, b2 := signIn(t, st, userAlice, id)
		if r := presentToken(t, c, btok2); r[keyOK] != true {
			t.Fatalf("auth = %v", r)
		}
		e.b.dev.DropBrowser(id, b2.ID)
		if r := deviceGets(t, c, protocol.TypeBrowserAuthResult); r[keyCode] != protocol.BrowserAuthInvalid {
			t.Fatalf("local drop = %v", r)
		}
	})
}

func TestBrowserToken(t *testing.T) {
	t.Parallel()
	stores(t, func(t *testing.T, st *store.Store) {
		e := setup(t, st)
		_, id := e.pair(e.a)
		ctx := context.Background()
		verifier := strings.Repeat("v", 43)
		redirect := protocol.BrowserRedirectURL(protocol.BrowserExtensionID)
		grant := hub.BrowserGrant{User: userAlice, DeviceID: id, InstanceID: instance, Browser: firefoxUA,
			Challenge: device.S256(verifier), RedirectURI: redirect}
		body := func(code, v string) map[string]string {
			return map[string]string{"grant_type": "authorization_code", keyCode: code, "code_verifier": v,
				"redirect_uri": redirect, keyDeviceID: id, "instance": instance}
		}
		u := e.a.url + "/api/browser/token"

		code, _ := st.CreateBrowserCode(ctx, grant)
		if s, out := call(t, http.MethodPost, u, "", body(code, strings.Repeat("w", 43))); s != http.StatusBadRequest || out[keyError] != "invalid_grant" {
			t.Fatalf("wrong verifier = %d %v", s, out)
		}
		if s, _ := call(t, http.MethodPost, u, "", body(code, verifier)); s != http.StatusBadRequest {
			t.Fatalf("code after a wrong verifier = %d", s)
		}
		code, _ = st.CreateBrowserCode(ctx, grant)
		b := body(code, verifier)
		b["instance"] = "instance-other-000000"
		if s, _ := call(t, http.MethodPost, u, "", b); s != http.StatusBadRequest {
			t.Fatalf("other instance = %d", s)
		}
		code, _ = st.CreateBrowserCode(ctx, grant)
		s, out := call(t, http.MethodPost, u, "", body(code, verifier))
		if s != http.StatusOK || out["user"] != userAlice || out[keyDeviceID] != id || out["device_name"] != deviceName {
			t.Fatalf("token = %d %v", s, out)
		}
		btok := str(t, out, keyToken)
		if sess, ok, _ := st.BrowserSessionByToken(ctx, btok); !ok || sess.ID != out["session_id"] {
			t.Fatalf("issued token unknown: %v", ok)
		}
		if s, _ := call(t, http.MethodPost, u, "", body(code, verifier)); s != http.StatusBadRequest {
			t.Fatalf("reused code = %d", s)
		}
		bobGrant := grant
		bobGrant.User = userBob
		code, _ = st.CreateBrowserCode(ctx, bobGrant)
		if s, _ := call(t, http.MethodPost, u, "", body(code, verifier)); s != http.StatusBadRequest {
			t.Fatalf("code of another user for alice's device = %d", s)
		}
	})
}

// forgedDropped checks that a browser_auth_result from the plugin, a
// message with a case-variant type member and messages with a second,
// null type never reach the device.
func forgedDropped(t *testing.T, c *websocket.Conn, pc *devicetest.Conn) {
	t.Helper()
	if err := pc.Send(context.Background(), map[string]any{keyType: protocol.TypeBrowserAuthResult, keyOK: true, "user": userAlice}); err != nil {
		t.Fatal(err)
	}
	if err := pc.Send(context.Background(), map[string]any{keyType: protocol.TypeExec, "TYPE": protocol.TypeBrowser, "id": "x0"}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"type":"browser_auth_result","type":null,"ok":true,"user":"alice"}`,
		`{"type":"browser","type":null,"id":"x2","op":"tabs_list"}`,
		`{"type":"browser","id":"x3","op":"tabs_list","type":"exec"}`,
	} {
		if err := pc.SendRaw(context.Background(), []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if err := pc.Send(context.Background(), map[string]any{keyType: protocol.TypeExec, "id": "x1", "cmd": "pwd", keyCWD: deviceDir}); err != nil {
		t.Fatal(err)
	}
	if m := deviceGets(t, c, protocol.TypeExec); m["id"] != "x1" {
		t.Fatalf("the device got %v; the forged result or the case-variant message was not dropped", m)
	}
}

// revokedAtNextOp revokes sign-in id in the store and checks that the
// next browser request drops it.
func revokedAtNextOp(t *testing.T, st *store.Store, c *websocket.Conn, pc *devicetest.Conn, id string) {
	t.Helper()
	if _, ok, err := st.RevokeBrowserSession(context.Background(), userAlice, id); !ok || err != nil {
		t.Fatal(err)
	}
	sendBrowser(t, pc, "b3")
	if st3 := pluginGets(t, pc, protocol.TypeBrowserStatus); st3["connected"] != false || st3["signed_in"] != false ||
		!strings.Contains(str(t, st3, "reason"), "no longer valid") {
		t.Fatalf("status after revoke = %v", st3)
	}
	res := pluginGets(t, pc, protocol.TypeBrowserResult)
	if res[keyCode] != protocol.BrowserNotSignedIn || !strings.Contains(str(t, res, keyError), "no longer valid") {
		t.Fatalf("browser after revoke = %v", res)
	}
	if r := deviceGets(t, c, protocol.TypeBrowserAuthResult); r[keyOK] != false || r[keyCode] != protocol.BrowserAuthInvalid {
		t.Fatalf("drop notice = %v", r)
	}
	writeJSON(t, c, protocol.BrowserStatus{Type: protocol.TypeBrowserStatus})
	if st4 := pluginGets(t, pc, protocol.TypeBrowserStatus); st4["connected"] != false || st4["reason"] != nil {
		t.Fatalf("status of an extension that is not connected = %v", st4)
	}
}
