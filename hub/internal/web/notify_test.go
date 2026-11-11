package web_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/notify"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/web"
)

const (
	kEndpoint   = "endpoint"
	kAuth       = "auth"
	kKeys       = "keys"
	kP256dh     = "p256dh"
	kPicks      = "picks"
	groupFamily = "family"
	fcmEndpoint = "https://fcm.googleapis.com/fcm/send/abc"
)

type fakeNotifier struct{ tests int }

func (f *fakeNotifier) PushKey() string { return "BPublicKey" }

func (f *fakeNotifier) Test(context.Context, string) (notify.TestResult, error) {
	f.tests++
	if f.tests > 1 {
		return notify.TestResult{}, notify.ErrTooSoon
	}
	return notify.TestResult{Telegram: 1, Push: 2}, nil
}

func notifyEnv(t *testing.T) *hubEnv {
	t.Helper()
	return setupWith(t, func(_ *config.Config, o *web.Options, st *store.Store) {
		o.Notices, o.Notifier = st, &fakeNotifier{}
	})
}

func pushKeys(t *testing.T) (string, string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(auth)
}

type notifyState struct {
	Settings      hub.NotifySettings `json:"settings"`
	PushKey       string             `json:"push_key"`
	Subscriptions []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"subscriptions"`
}

func TestNotifySettingsAPI(t *testing.T) {
	t.Parallel()
	e := notifyEnv(t)
	c := client(t)
	m := login(t, e, c)
	resp, body := get(t, c, e.url+"/api/notify")
	var st notifyState
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &st) != nil || st.Settings != hub.DefaultNotifySettings() || st.PushKey == "" {
		t.Fatalf("state = %d %s", resp.StatusCode, body)
	}
	want := hub.NotifySettings{Approvals: true, Jobs: true, Push: true, QuietStart: "22:00", QuietEnd: "07:00", TimeZone: "America/Chicago"}
	if resp, _ := post(t, c, e.url+"/api/notify/settings", "", "", want); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("settings without CSRF = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, e.url+"/api/notify/settings", m.CSRF, "https://evil.example", want); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin settings = %d", resp.StatusCode)
	}
	if resp, body := post(t, c, e.url+"/api/notify/settings", m.CSRF, "", want); resp.StatusCode != http.StatusOK || !strings.Contains(body, "America/Chicago") {
		t.Fatalf("save = %d %s", resp.StatusCode, body)
	}
	if got, _ := e.store.NotifySettings(context.Background(), userAlice); got != want {
		t.Fatalf("stored = %+v", got)
	}
	bad := want
	bad.QuietEnd = ""
	if resp, _ := post(t, c, e.url+"/api/notify/settings", m.CSRF, "", bad); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("half quiet hours = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, e.url+"/api/notify/test", m.CSRF, "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("test = %d", resp.StatusCode)
	}
	if resp, _ := post(t, c, e.url+"/api/notify/test", m.CSRF, "", nil); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second test = %d", resp.StatusCode)
	}
}

func TestPushSubscriptionsArePerUser(t *testing.T) {
	t.Parallel()
	e := notifyEnv(t)
	alice := client(t)
	am := login(t, e, alice)
	p256dh, auth := pushKeys(t)
	sub := map[string]any{kEndpoint: fcmEndpoint, kKeys: map[string]string{kP256dh: p256dh, kAuth: auth}, "label": "Pixel"}
	if resp, _ := post(t, alice, e.url+"/api/notify/push", "", "", sub); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("subscribe without CSRF = %d", resp.StatusCode)
	}
	internal := map[string]any{kEndpoint: "https://honcho.internal.svc/v3", kKeys: map[string]string{kP256dh: p256dh, kAuth: auth}}
	if resp, _ := post(t, alice, e.url+"/api/notify/push", am.CSRF, "", internal); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("internal endpoint = %d", resp.StatusCode)
	}
	resp, body := post(t, alice, e.url+"/api/notify/push", am.CSRF, "", sub)
	var st notifyState
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &st) != nil || len(st.Subscriptions) != 1 || st.Subscriptions[0].Label != "Pixel" {
		t.Fatalf("subscribe = %d %s", resp.StatusCode, body)
	}
	if _, body := post(t, alice, e.url+"/api/notify/push/check", am.CSRF, "", map[string]string{kEndpoint: fcmEndpoint}); !strings.Contains(body, `"mine":true`) {
		t.Fatalf("check = %s", body)
	}
	long := map[string]any{kEndpoint: fcmEndpoint + strings.Repeat("a", 3000), kKeys: map[string]string{kP256dh: p256dh, kAuth: auth}}
	if resp, _ := post(t, alice, e.url+"/api/notify/push", am.CSRF, "", long); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("long endpoint = %d", resp.StatusCode)
	}
	if strings.Contains(body, "fcm.googleapis.com") || strings.Contains(body, auth) {
		t.Fatalf("state shows the endpoint or keys: %s", body)
	}
	e.is.mu.Lock()
	e.is.user, e.is.groups = userBob, []string{groupFamily}
	e.is.mu.Unlock()
	bob := client(t)
	bm := login(t, e, bob)
	if _, body := get(t, bob, e.url+"/api/notify"); strings.Contains(body, "Pixel") {
		t.Fatalf("bob sees alice's devices: %s", body)
	}
	_, _ = post(t, bob, e.url+"/api/notify/push/remove", bm.CSRF, "", map[string]string{"id": st.Subscriptions[0].ID})
	if _, body := post(t, bob, e.url+"/api/notify/push/check", bm.CSRF, "", map[string]string{kEndpoint: fcmEndpoint}); !strings.Contains(body, `"mine":false`) {
		t.Fatalf("bob check = %s", body)
	}
	p2, a2 := pushKeys(t)
	steal := map[string]any{kEndpoint: fcmEndpoint, kKeys: map[string]string{kP256dh: p2, kAuth: a2}}
	if resp, _ := post(t, bob, e.url+"/api/notify/push", bm.CSRF, "", steal); resp.StatusCode != http.StatusConflict {
		t.Fatalf("bob took alice's endpoint = %d", resp.StatusCode)
	}
	if subs, _ := e.store.PushSubscriptions(context.Background(), userAlice); len(subs) != 1 {
		t.Fatalf("bob removed alice's subscription: %+v", subs)
	}
	if resp, _ := post(t, bob, e.url+"/api/presence", bm.CSRF, "", map[string]any{"backend": "pc", "session": "s1", "visible": true}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob presence on alice's backend = %d", resp.StatusCode)
	}
	if resp, _ := post(t, alice, e.url+"/api/presence", am.CSRF, "", map[string]any{"backend": "pc", "session": "s1", "visible": true}); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("presence = %d", resp.StatusCode)
	}
	if ok, _ := e.store.Present(context.Background(), userAlice, "pc", "s1", time.Now().Add(-time.Minute)); !ok {
		t.Fatal("presence not stored")
	}
	if resp, _ := post(t, alice, e.url+"/api/notify/push/remove", am.CSRF, "", map[string]string{kEndpoint: fcmEndpoint}); resp.StatusCode != http.StatusOK {
		t.Fatalf("remove = %d", resp.StatusCode)
	}
	if subs, _ := e.store.PushSubscriptions(context.Background(), userAlice); len(subs) != 0 {
		t.Fatalf("not removed: %+v", subs)
	}
}

func TestAnswerQuestion(t *testing.T) {
	t.Parallel()
	e := notifyEnv(t)
	alice := client(t)
	am := login(t, e, alice)
	if resp, _ := post(t, alice, e.url+"/api/sessions/pc/s1/prompt", am.CSRF, "", map[string]string{kText: "ask me"}); resp.StatusCode >= 300 {
		t.Fatalf("prompt = %d", resp.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := e.svc.PendingPrompt(context.Background(), userAlice, "pc", "s1", "cl-1")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("question never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.is.mu.Lock()
	e.is.user, e.is.groups = userBob, []string{groupFamily}
	e.is.mu.Unlock()
	bob := client(t)
	bm := login(t, e, bob)
	if resp, _ := post(t, bob, e.url+"/api/sessions/pc/s1/prompts/cl-1/answer", bm.CSRF, "", map[string]any{kPicks: []string{"red"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bob answer = %d", resp.StatusCode)
	}
	if resp, _ := post(t, alice, e.url+"/api/sessions/pc/s1/prompts/cl-1/answer", am.CSRF, "", map[string]any{kPicks: []string{"purple"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad pick = %d", resp.StatusCode)
	}
	if resp, body := post(t, alice, e.url+"/api/sessions/pc/s1/prompts/cl-1/answer", am.CSRF, "", map[string]any{kPicks: []string{"red"}}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("answer = %d %s", resp.StatusCode, body)
	}
}

func TestPWAFiles(t *testing.T) {
	t.Parallel()
	e := setup(t)
	c := client(t)
	resp, body := get(t, c, e.url+"/sw.js")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("sw.js = %d %v", resp.StatusCode, resp.Header)
	}
	if strings.Contains(body, "/api/") || strings.Contains(body, "cache.put") {
		t.Fatal("the service worker touches API answers")
	}
	resp, body = get(t, c, e.url+"/manifest.webmanifest")
	var mf struct {
		StartURL string `json:"start_url"`
		Icons    []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/manifest+json" || json.Unmarshal([]byte(body), &mf) != nil || mf.StartURL != "/" {
		t.Fatalf("manifest = %d %s", resp.StatusCode, body)
	}
	for _, ic := range mf.Icons {
		if r, _ := get(t, c, e.url+ic.Src); r.StatusCode != http.StatusOK {
			t.Fatalf("icon %s = %d", ic.Src, r.StatusCode)
		}
	}
	if r, _ := get(t, c, e.url+"/static/offline.html"); r.StatusCode != http.StatusOK {
		t.Fatalf("offline page = %d", r.StatusCode)
	}
}
