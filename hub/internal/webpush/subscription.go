package webpush

import (
	"crypto/ecdh"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Subscription is a browser PushSubscription. P256dh and Auth are as the
// browser gives them (base64url, maybe padded).
type Subscription struct {
	Endpoint string
	P256dh   string
	Auth     string
}

// Validate checks the keys decode (p256dh a valid 65-byte uncompressed P-256
// point, auth 16 bytes) and the endpoint passes ValidEndpoint.
func (s Subscription) Validate() error {
	if _, _, err := s.keys(); err != nil {
		return err
	}
	return ValidEndpoint(s.Endpoint)
}

func (s Subscription) keys() (*ecdh.PublicKey, []byte, error) {
	p, err := decodeB64URL(s.P256dh)
	if err != nil {
		return nil, nil, fmt.Errorf("webpush: p256dh: %w", err)
	}
	if len(p) != 65 || p[0] != 4 {
		return nil, nil, errors.New("webpush: p256dh is not a 65-byte uncompressed point")
	}
	pub, err := ecdh.P256().NewPublicKey(p)
	if err != nil {
		return nil, nil, fmt.Errorf("webpush: p256dh: %w", err)
	}
	auth, err := decodeB64URL(s.Auth)
	if err != nil {
		return nil, nil, fmt.Errorf("webpush: auth: %w", err)
	}
	if len(auth) != 16 {
		return nil, nil, fmt.Errorf("webpush: auth is %d bytes, want 16", len(auth))
	}
	return pub, auth, nil
}

// ValidEndpoint allows only https URLs on the default port, without userinfo,
// whose host is a known push service: fcm.googleapis.com,
// android.googleapis.com, updates.push.services.mozilla.com and any subdomain
// of push.services.mozilla.com, web.push.apple.com and any subdomain of
// push.apple.com, and any subdomain of notify.windows.com. This blocks SSRF to
// internal hosts.
func ValidEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webpush: endpoint: %w", err)
	}
	if u.Scheme != "https" {
		return errors.New("webpush: endpoint must use https")
	}
	if u.Opaque != "" || u.Host == "" {
		return errors.New("webpush: endpoint has no host")
	}
	if u.User != nil {
		return errors.New("webpush: endpoint must not carry userinfo")
	}
	if p := u.Port(); p != "" && p != "443" {
		return errors.New("webpush: endpoint must use the default port")
	}
	if !knownPushHost(u.Hostname()) {
		return fmt.Errorf("webpush: endpoint host %q is not a known push service", u.Hostname())
	}
	return nil
}

func knownPushHost(h string) bool {
	for _, c := range h {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '.' {
			return false
		}
	}
	switch h {
	case "fcm.googleapis.com", "android.googleapis.com", "updates.push.services.mozilla.com", "web.push.apple.com":
		return true
	}
	for _, parent := range [...]string{"push.services.mozilla.com", "push.apple.com", "notify.windows.com"} {
		if sub, ok := strings.CutSuffix(h, "."+parent); ok && validLabels(sub) {
			return true
		}
	}
	return false
}

func validLabels(s string) bool {
	for l := range strings.SplitSeq(s, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
	}
	return true
}
