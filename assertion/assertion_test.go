package assertion_test

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/assertion"
)

func expect() assertion.Expect {
	return assertion.Expect{Audience: "backend-a", Leg: assertion.LegExec, Method: http.MethodGet, Path: "/api/plugins/device/ws"}
}

func claims(now time.Time) assertion.Claims {
	return assertion.Claims{
		Aud: "backend-a", Leg: assertion.LegExec, Method: http.MethodGet, Path: "/api/plugins/device/ws",
		Sub: "user-1", DeviceID: "dev_1", DeviceName: "desktop",
		IssuedAt: now.Unix(), Expires: now.Add(30 * time.Second).Unix(),
	}
}

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestSignVerify(t *testing.T) {
	t.Parallel()
	pub, priv := keypair(t)
	now := time.Unix(1_800_000_000, 0)
	v := assertion.NewVerifier([]ed25519.PublicKey{pub})
	v.Now = func() time.Time { return now }
	enc, err := assertion.Sign(priv, claims(now))
	if err != nil {
		t.Fatal(err)
	}
	c, err := v.Verify(enc, expect())
	if err != nil {
		t.Fatal(err)
	}
	if c.DeviceID != "dev_1" || c.Typ != assertion.Type {
		t.Errorf("claims = %+v", c)
	}
	if _, err := v.Verify(enc, expect()); !errors.Is(err, assertion.ErrReplay) {
		t.Errorf("replay: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	t.Parallel()
	pub, priv := keypair(t)
	_, other := keypair(t)
	now := time.Unix(1_800_000_000, 0)
	cases := map[string]struct {
		key    ed25519.PrivateKey
		mutate func(*assertion.Claims)
		exp    assertion.Expect
		err    error
	}{
		"wrong key":      {other, nil, expect(), assertion.ErrSignature},
		"wrong audience": {priv, func(c *assertion.Claims) { c.Aud = "backend-b" }, expect(), assertion.ErrClaims},
		"wrong leg":      {priv, func(c *assertion.Claims) { c.Leg = assertion.LegACP }, expect(), assertion.ErrClaims},
		"wrong path":     {priv, func(c *assertion.Claims) { c.Path = "/api/ws" }, expect(), assertion.ErrClaims},
		"bad device id":  {priv, func(c *assertion.Claims) { c.DeviceID = "a/b" }, expect(), assertion.ErrClaims},
		"too long":       {priv, func(c *assertion.Claims) { c.Expires = c.IssuedAt + 31 }, expect(), assertion.ErrExpired},
		"expired": {priv, func(c *assertion.Claims) {
			c.IssuedAt, c.Expires = now.Add(-60*time.Second).Unix(), now.Add(-40*time.Second).Unix()
		}, expect(), assertion.ErrExpired},
		"future": {priv, func(c *assertion.Claims) {
			c.IssuedAt, c.Expires = now.Add(20*time.Second).Unix(), now.Add(40*time.Second).Unix()
		}, expect(), assertion.ErrExpired},
		"empty audience expected": {priv, nil, assertion.Expect{Leg: assertion.LegExec, Method: http.MethodGet, Path: expect().Path}, assertion.ErrClaims},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			v := assertion.NewVerifier([]ed25519.PublicKey{pub})
			v.Now = func() time.Time { return now }
			c := claims(now)
			if tc.mutate != nil {
				tc.mutate(&c)
			}
			enc, err := assertion.Sign(tc.key, c)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := v.Verify(enc, tc.exp); !errors.Is(err, tc.err) {
				t.Errorf("got %v, want %v", err, tc.err)
			}
		})
	}
}

func TestMalformed(t *testing.T) {
	t.Parallel()
	pub, priv := keypair(t)
	now := time.Now()
	enc, err := assertion.Sign(priv, claims(now))
	if err != nil {
		t.Fatal(err)
	}
	v := assertion.NewVerifier([]ed25519.PublicKey{pub})
	for _, bad := range []string{"", "v2." + enc[3:], enc + ".x", strings.Replace(enc, ".", "!", 2), enc[:len(enc)-4]} {
		if _, err := v.Verify(bad, expect()); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	tampered := []byte(enc)
	tampered[10] ^= 1
	if _, err := v.Verify(string(tampered), expect()); err == nil {
		t.Error("accepted tampered payload")
	}
	if _, err := assertion.NewVerifier(nil).Verify(enc, expect()); !errors.Is(err, assertion.ErrNoKeys) {
		t.Errorf("no keys: %v", err)
	}
}

func TestParsePublicKeys(t *testing.T) {
	t.Parallel()
	pub1, _ := keypair(t)
	pub2, _ := keypair(t)
	der, err := x509.MarshalPKIXPublicKey(pub1)
	if err != nil {
		t.Fatal(err)
	}
	text := base64.RawURLEncoding.EncodeToString(pub1) + "\n" + string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})) +
		"# rotated key\n" + base64.StdEncoding.EncodeToString(pub2) + "\n\n"
	keys, err := assertion.ParsePublicKeys(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 || !keys[0].Equal(pub1) || !keys[1].Equal(pub1) || !keys[2].Equal(pub2) {
		t.Errorf("keys = %v", keys)
	}
	if _, err := assertion.ParsePublicKeys("\n# none\n"); !errors.Is(err, assertion.ErrNoKeys) {
		t.Errorf("empty: %v", err)
	}
	if _, err := assertion.ParsePublicKeys("abc\n"); !errors.Is(err, assertion.ErrMalformed) {
		t.Errorf("short: %v", err)
	}
}
