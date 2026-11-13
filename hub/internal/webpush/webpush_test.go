package webpush_test

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/webpush"
)

const (
	rfcPlain     = "When I grow up, I want to be a watermelon"
	rfcUAPriv    = "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"
	rfcUAPub     = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	rfcASPriv    = "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"
	rfcSalt      = "DGv6ra1nlYgDCS1FRnbzlw"
	rfcAuth      = "BTBZMqHH6r4Tts7J_aSIgg"
	rfcBody      = "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	testSubject  = "mailto:ops@example.com"
	pkcs8PEM     = "PRIVATE KEY"
	testEndpoint = "https://fcm.googleapis.com/fcm/send/abc"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

func TestRFC8291Vector(t *testing.T) {
	t.Parallel()
	sub := webpush.Subscription{Endpoint: testEndpoint, P256dh: rfcUAPub, Auth: rfcAuth}
	got, err := webpush.EncryptWith(sub, []byte(rfcPlain), b64(t, rfcSalt), b64(t, rfcASPriv))
	if err != nil {
		t.Fatal(err)
	}
	if want := b64(t, rfcBody); !bytes.Equal(got, want) {
		t.Fatalf("body mismatch\n got %s\nwant %s", base64.RawURLEncoding.EncodeToString(got), rfcBody)
	}
	ua, err := ecdh.P256().NewPrivateKey(b64(t, rfcUAPriv))
	if err != nil {
		t.Fatal(err)
	}
	if plain := decrypt(t, ua, b64(t, rfcAuth), got); string(plain) != rfcPlain {
		t.Fatalf("decrypted %q", plain)
	}
}

func TestEncryptRejectsLargePayload(t *testing.T) {
	t.Parallel()
	sub := webpush.Subscription{Endpoint: testEndpoint, P256dh: rfcUAPub, Auth: rfcAuth}
	if _, err := webpush.EncryptWith(sub, make([]byte, webpush.MaxPayload), b64(t, rfcSalt), b64(t, rfcASPriv)); err != nil {
		t.Fatalf("payload at limit: %v", err)
	}
	if _, err := webpush.EncryptWith(sub, make([]byte, webpush.MaxPayload+1), b64(t, rfcSalt), b64(t, rfcASPriv)); err == nil {
		t.Fatal("payload over limit accepted")
	}
}

// decrypt is an independent user agent side of RFC 8291 and RFC 8188.
func decrypt(t *testing.T, ua *ecdh.PrivateKey, auth, body []byte) []byte {
	t.Helper()
	if len(body) < 21 {
		t.Fatalf("body too short: %d", len(body))
	}
	salt := body[:16]
	if rs := binary.BigEndian.Uint32(body[16:20]); rs != 4096 {
		t.Fatalf("record size %d", rs)
	}
	idlen := int(body[20])
	if idlen != 65 {
		t.Fatalf("idlen %d", idlen)
	}
	asPubBytes := body[21 : 21+idlen]
	ct := body[21+idlen:]
	asPub, err := ecdh.P256().NewPublicKey(asPubBytes)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ua.ECDH(asPub)
	if err != nil {
		t.Fatal(err)
	}
	info := append(append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...), asPubBytes...)
	prkKey, err := hkdf.Extract(sha256.New, shared, auth)
	if err != nil {
		t.Fatal(err)
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(info), 32)
	if err != nil {
		t.Fatal(err)
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		t.Fatal(err)
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	i := bytes.LastIndexFunc(plain, func(r rune) bool { return r != 0 })
	if i < 0 || plain[i] != 0x02 {
		t.Fatalf("missing 0x02 delimiter in last record")
	}
	return plain[:i]
}

type captured struct {
	mu     sync.Mutex
	header http.Header
	body   []byte
	host   string
	hits   int
}

func newServer(t *testing.T, status int) (*httptest.Server, *captured) {
	t.Helper()
	c := &captured{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.header = r.Header.Clone()
		c.body = b
		c.host = r.Host
		c.hits++
		c.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte("nope"))
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func newUA(t *testing.T) (*ecdh.PrivateKey, []byte, webpush.Subscription) {
	t.Helper()
	ua, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return ua, auth, webpush.Subscription{
		P256dh: base64.URLEncoding.EncodeToString(ua.PublicKey().Bytes()),
		Auth:   base64.URLEncoding.EncodeToString(auth),
	}
}

func newVAPID(t *testing.T) *webpush.VAPID {
	t.Helper()
	k, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	v, err := webpush.ParseKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSendRoundTrip(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.StatusCreated)
	ua, auth, sub := newUA(t)
	sub.Endpoint = srv.URL + "/push/xyz"
	v := newVAPID(t)
	webpush.AllowAnyEndpoint(v)
	payload := []byte(`{"title":"hi","body":"round trip"}`)
	before := time.Now()
	err := v.Send(context.Background(), srv.Client(), sub, testSubject, webpush.Message{
		Payload: payload, TTL: 90*time.Second + 500*time.Millisecond, Urgency: "high", Topic: "chat-42",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if got := decrypt(t, ua, auth, c.body); !bytes.Equal(got, payload) {
		t.Fatalf("payload %q", got)
	}
	for k, want := range map[string]string{
		"Content-Encoding": "aes128gcm",
		"Ttl":              "90",
		"Urgency":          "high",
		"Topic":            "chat-42",
	} {
		if got := c.header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	checkAuth(t, c.header.Get("Authorization"), v.PublicKey(), "https://"+c.host, before)
}

func checkAuth(t *testing.T, header, pubB64, aud string, before time.Time) {
	t.Helper()
	rest, ok := strings.CutPrefix(header, "vapid t=")
	if !ok {
		t.Fatalf("authorization %q", header)
	}
	jwt, k, ok := strings.Cut(rest, ", k=")
	if !ok || k != pubB64 {
		t.Fatalf("authorization %q, k want %q", header, pubB64)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	var hdr map[string]string
	if err := json.Unmarshal(b64(t, parts[0]), &hdr); err != nil {
		t.Fatal(err)
	}
	if hdr["alg"] != "ES256" || hdr["typ"] != "JWT" {
		t.Fatalf("jwt header %v", hdr)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), b64(t, pubB64))
	if err != nil {
		t.Fatal(err)
	}
	sig := b64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes", len(sig))
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("jwt signature does not verify")
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(b64(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != aud || claims.Sub != testSubject {
		t.Fatalf("claims %+v, want aud %q", claims, aud)
	}
	lo, hi := before.Add(12*time.Hour).Unix(), time.Now().Add(12*time.Hour).Unix()
	if claims.Exp < lo || claims.Exp > hi {
		t.Fatalf("exp %d outside [%d, %d]", claims.Exp, lo, hi)
	}
}

func TestSendGone(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusGone, http.StatusNotFound} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Parallel()
			srv, _ := newServer(t, status)
			_, _, sub := newUA(t)
			sub.Endpoint = srv.URL + "/push/old"
			v := newVAPID(t)
			webpush.AllowAnyEndpoint(v)
			err := v.Send(context.Background(), srv.Client(), sub, testSubject, webpush.Message{Payload: []byte("x")})
			if !errors.Is(err, webpush.ErrGone) {
				t.Fatalf("err = %v, want ErrGone", err)
			}
		})
	}
}

func TestSendStatusError(t *testing.T) {
	t.Parallel()
	srv, _ := newServer(t, http.StatusTooManyRequests)
	_, _, sub := newUA(t)
	sub.Endpoint = srv.URL
	v := newVAPID(t)
	webpush.AllowAnyEndpoint(v)
	err := v.Send(context.Background(), srv.Client(), sub, testSubject, webpush.Message{})
	var se *webpush.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusTooManyRequests || errors.Is(err, webpush.ErrGone) {
		t.Fatalf("err = %v", err)
	}
}

func TestSendNoRedirect(t *testing.T) {
	t.Parallel()
	target, tc := newServer(t, http.StatusCreated)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	_, _, sub := newUA(t)
	sub.Endpoint = srv.URL
	v := newVAPID(t)
	webpush.AllowAnyEndpoint(v)
	err := v.Send(context.Background(), srv.Client(), sub, testSubject, webpush.Message{})
	var se *webpush.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v, want 307 StatusError", err)
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if tc.hits != 0 {
		t.Fatal("redirect was followed")
	}
}

func TestSendValidatesEndpoint(t *testing.T) {
	t.Parallel()
	srv, c := newServer(t, http.StatusCreated)
	_, _, sub := newUA(t)
	sub.Endpoint = srv.URL
	err := newVAPID(t).Send(context.Background(), srv.Client(), sub, testSubject, webpush.Message{})
	if err == nil {
		t.Fatal("send to httptest endpoint without bypass succeeded")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hits != 0 {
		t.Fatal("request reached the server")
	}
}

func TestSendRejectsBadInput(t *testing.T) {
	t.Parallel()
	_, _, sub := newUA(t)
	sub.Endpoint = testEndpoint
	v := newVAPID(t)
	cases := map[string]struct {
		subject string
		m       webpush.Message
	}{
		"subject":   {"ops@example.com", webpush.Message{}},
		"payload":   {testSubject, webpush.Message{Payload: make([]byte, webpush.MaxPayload+1)}},
		"urgency":   {testSubject, webpush.Message{Urgency: "urgent"}},
		"topic len": {testSubject, webpush.Message{Topic: strings.Repeat("a", 33)}},
		"topic chr": {testSubject, webpush.Message{Topic: "a b"}},
		"ttl":       {testSubject, webpush.Message{TTL: -time.Second}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := v.Send(context.Background(), http.DefaultClient, sub, tc.subject, tc.m); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestValidEndpoint(t *testing.T) {
	t.Parallel()
	allow := []string{
		"https://fcm.googleapis.com/fcm/send/abc:def",
		"https://android.googleapis.com/gcm/send/abc",
		"https://updates.push.services.mozilla.com/wpush/v2/gAAA",
		"https://foo.push.services.mozilla.com/wpush/v2/x",
		"https://web.push.apple.com/QGx",
		"https://api.push.apple.com/3/device/x",
		"https://wns2-by3p.notify.windows.com/w/?token=abc",
		"https://fcm.googleapis.com:443/fcm/send/abc",
	}
	deny := []string{
		"http://fcm.googleapis.com/fcm/send/abc",
		"https://127.0.0.1/push",
		"https://[::1]/push",
		"https://10.0.0.5/push",
		"https://169.254.169.254/latest/meta-data",
		"https://localhost/push",
		"https://hub.internal/push",
		"https://fcm.googleapis.com.evil.example/push",
		"https://evilfcm.googleapis.com/push",
		"https://evil.example/fcm.googleapis.com",
		"https://notify.windows.com/x",
		"https://push.apple.com/x",
		"https://push.services.mozilla.com/x",
		"https://evilpush.apple.com/x",
		"https://user:pass@fcm.googleapis.com/fcm/send/abc",
		"https://user@fcm.googleapis.com/fcm/send/abc",
		"https://fcm.googleapis.com:8443/fcm/send/abc",
		"https://fcm.googleapis.com:80/fcm/send/abc",
		"https://fcm.googleapis.com./fcm/send/abc",
		"https://a..push.apple.com/x",
		"https://-a.push.apple.com/x",
		"https:fcm.googleapis.com",
		"ftp://fcm.googleapis.com/x",
		"//fcm.googleapis.com/x",
		"",
		"not a url",
	}
	for _, u := range allow {
		if err := webpush.ValidEndpoint(u); err != nil {
			t.Errorf("ValidEndpoint(%q) = %v, want nil", u, err)
		}
	}
	for _, u := range deny {
		if err := webpush.ValidEndpoint(u); err == nil {
			t.Errorf("ValidEndpoint(%q) = nil, want error", u)
		}
	}
}

func TestSubscriptionValidate(t *testing.T) {
	t.Parallel()
	_, _, good := newUA(t)
	good.Endpoint = testEndpoint
	if err := good.Validate(); err != nil {
		t.Fatalf("good: %v", err)
	}
	raw := good
	raw.P256dh = strings.TrimRight(raw.P256dh, "=")
	raw.Auth = strings.TrimRight(raw.Auth, "=")
	if err := raw.Validate(); err != nil {
		t.Fatalf("unpadded: %v", err)
	}
	offCurve := make([]byte, 65)
	offCurve[0] = 4
	offCurve[64] = 1
	bad := map[string]webpush.Subscription{
		"endpoint":  {Endpoint: "https://127.0.0.1/x", P256dh: good.P256dh, Auth: good.Auth},
		"p256dh":    {Endpoint: testEndpoint, P256dh: "!!", Auth: good.Auth},
		"short key": {Endpoint: testEndpoint, P256dh: good.Auth, Auth: good.Auth},
		"off curve": {Endpoint: testEndpoint, P256dh: base64.RawURLEncoding.EncodeToString(offCurve), Auth: good.Auth},
		"auth len":  {Endpoint: testEndpoint, P256dh: good.P256dh, Auth: "AAAA"},
	}
	for name, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate accepted", name)
		}
	}
}

func TestParseKey(t *testing.T) {
	t.Parallel()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := k.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	wantPub := base64.RawURLEncoding.EncodeToString(pub)
	p8, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	sec1, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	scalar, err := k.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	params := pem.EncodeToMemory(&pem.Block{Type: "EC PARAMETERS", Bytes: []byte{6, 8, 42, 134, 72, 206, 61, 3, 1, 7}})
	inputs := map[string]string{
		"pkcs8":         string(pem.EncodeToMemory(&pem.Block{Type: pkcs8PEM, Bytes: p8})),
		"sec1":          string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})),
		"sec1 w/params": string(params) + string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})),
		"raw":           base64.RawURLEncoding.EncodeToString(scalar),
		"raw padded":    base64.URLEncoding.EncodeToString(scalar) + "\n",
	}
	for name, in := range inputs {
		v, err := webpush.ParseKey(in)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := v.PublicKey(); got != wantPub {
			t.Errorf("%s: public key %q, want %q", name, got, wantPub)
		}
	}

	if v, err := webpush.ParseKey(rfcASPriv); err != nil || v.PublicKey() != "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8" {
		t.Errorf("RFC sender key: %v", err)
	}

	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384der, err := x509.MarshalPKCS8PrivateKey(p384)
	if err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]string{
		"empty":     "",
		"garbage":   "not-a-key!",
		"short raw": base64.RawURLEncoding.EncodeToString(scalar[:31]),
		"zero raw":  base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		"p384":      string(pem.EncodeToMemory(&pem.Block{Type: pkcs8PEM, Bytes: p384der})),
		"cert pem":  string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1}})),
	} {
		if _, err := webpush.ParseKey(in); err == nil {
			t.Errorf("%s: ParseKey accepted", name)
		}
	}
}

func TestGenerateKeyIsPKCS8(t *testing.T) {
	t.Parallel()
	s, err := webpush.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := pem.Decode([]byte(s))
	if b == nil || b.Type != pkcs8PEM {
		t.Fatalf("GenerateKey output %q", s)
	}
	if len(newVAPID(t).PublicKey()) != 87 {
		t.Fatal("public key is not 65 bytes base64url")
	}
}

func TestIsKeylessChromium(t *testing.T) {
	t.Parallel()
	for u, want := range map[string]bool{
		"https://jmt17.google.com/fcm/send/abc":     true,
		"https://jmt17.google.com:443/fcm/send/abc": true,
		"https://fcm.googleapis.com/fcm/send/abc":   false,
		"https://evil.jmt17.google.com/x":           false,
		"https://jmt17.google.com.evil.example/x":   false,
		"": false,
	} {
		if got := webpush.IsKeylessChromium(u); got != want {
			t.Errorf("IsKeylessChromium(%q) = %v, want %v", u, got, want)
		}
	}
	// Still not an endpoint the hub sends to.
	if err := webpush.ValidEndpoint("https://jmt17.google.com/fcm/send/abc"); err == nil {
		t.Error("ValidEndpoint accepts the keyless Chromium host")
	}
}
