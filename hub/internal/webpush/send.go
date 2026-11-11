package webpush

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Message is one push message.
type Message struct {
	Payload []byte
	// TTL is how long the push service keeps an undelivered message. It is
	// sent in whole seconds.
	TTL time.Duration
	// Urgency is "", "very-low", "low", "normal" or "high".
	Urgency string
	// Topic optionally replaces a pending message with the same topic. It is
	// at most 32 base64url characters.
	Topic string
}

// ErrGone means the push service says the subscription no longer exists (404
// or 410): the caller deletes it.
var ErrGone = errors.New("webpush: subscription gone")

// StatusError is a non-2xx answer from the push service other than 404 or 410.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("webpush: push service returned %d: %s", e.StatusCode, e.Body)
}

const (
	jwtLifetime   = 12 * time.Hour
	maxRespBody   = 4096
	defaultClient = 30 * time.Second
)

// Send encrypts the message and POSTs it to the subscription's endpoint.
// subject is the VAPID "sub" claim (a mailto: or https: URL). Send re-checks
// ValidEndpoint, never follows redirects, and returns ErrGone on 404 or 410
// and a *StatusError for other non-2xx answers. A nil client uses one with a
// 30 second timeout.
func (v *VAPID) Send(ctx context.Context, client *http.Client, sub Subscription, subject string, m Message) error {
	if !v.allowAnyEndpoint {
		if err := ValidEndpoint(sub.Endpoint); err != nil {
			return err
		}
	}
	u, err := url.Parse(sub.Endpoint)
	if err != nil {
		return fmt.Errorf("webpush: endpoint: %w", err)
	}
	if !strings.HasPrefix(subject, "mailto:") && !strings.HasPrefix(subject, "https:") {
		return errors.New("webpush: subject must be a mailto: or https: URL")
	}
	if err := checkMessage(m); err != nil {
		return err
	}
	uaPub, auth, err := sub.keys()
	if err != nil {
		return err
	}
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("webpush: ephemeral key: %w", err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("webpush: salt: %w", err)
	}
	body, err := encrypt(uaPub, auth, m.Payload, salt, asPriv)
	if err != nil {
		return err
	}
	aud := u.Scheme + "://" + u.Host
	if u.Port() == "443" {
		aud = u.Scheme + "://" + u.Hostname()
	}
	jwt, err := v.signJWT(aud, subject, time.Now().Add(jwtLifetime))
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webpush: request: %w", err)
	}
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+v.PublicKey())
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.FormatInt(int64(m.TTL/time.Second), 10))
	if m.Urgency != "" {
		req.Header.Set("Urgency", m.Urgency)
	}
	if m.Topic != "" {
		req.Header.Set("Topic", m.Topic)
	}

	c := http.Client{Timeout: defaultClient}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("webpush: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBody))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return fmt.Errorf("%w (status %d)", ErrGone, resp.StatusCode)
	default:
		return &StatusError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(msg))}
	}
}

func checkMessage(m Message) error {
	if len(m.Payload) > MaxPayload {
		return fmt.Errorf("webpush: payload is %d bytes, limit %d", len(m.Payload), MaxPayload)
	}
	if m.TTL < 0 {
		return errors.New("webpush: negative TTL")
	}
	switch m.Urgency {
	case "", "very-low", "low", "normal", "high":
	default:
		return fmt.Errorf("webpush: invalid urgency %q", m.Urgency)
	}
	if len(m.Topic) > 32 {
		return errors.New("webpush: topic is longer than 32 characters")
	}
	for _, c := range m.Topic {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return errors.New("webpush: topic must use the base64url alphabet")
		}
	}
	return nil
}

func (v *VAPID) signJWT(aud, sub string, exp time.Time) (string, error) {
	claims, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{aud, exp.Unix(), sub})
	if err != nil {
		return "", fmt.Errorf("webpush: jwt claims: %w", err)
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`)) + "." + enc.EncodeToString(claims)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, v.priv, h[:])
	if err != nil {
		return "", fmt.Errorf("webpush: jwt sign: %w", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + enc.EncodeToString(sig), nil
}
