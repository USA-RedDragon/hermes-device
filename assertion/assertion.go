// Package assertion signs and verifies the hub assertion: a short-lived
// Ed25519-signed statement from hermes-hub that one WebSocket upgrade to a
// user's Hermes backend comes from one paired device.
//
// The hub holds the private key. The Hermes pod (the device plugin and
// acp-host) holds only public keys, so a compromised pod cannot mint
// assertions for other backends or devices. docs/protocol.md is the
// normative description; the Python verifier in plugin/device/assertion.py
// must accept exactly what Verify accepts.
package assertion

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Header is the HTTP request header that carries the assertion on the
// upgrade request from the hub to the backend.
const Header = "X-Hermes-Device-Assertion"

// Prefix starts every encoded assertion and names its format version.
const Prefix = "v1."

// Type is the value of the typ field.
const Type = "hermes-device-assertion"

// MaxLifetime is the longest allowed exp - iat.
const MaxLifetime = 30 * time.Second

// ClockSkew is the clock difference a verifier tolerates on iat and exp.
const ClockSkew = 5 * time.Second

// MaxEncodedLen is the longest header value a verifier reads.
const MaxEncodedLen = 4096

// Legs name the endpoint an assertion is for.
const (
	LegExec = "exec"
	LegACP  = "acp"
)

// Errors returned by Verify and ParsePublicKeys.
var (
	ErrMalformed = errors.New("malformed assertion")
	ErrSignature = errors.New("assertion signature does not verify")
	ErrClaims    = errors.New("assertion claims refused")
	ErrExpired   = errors.New("assertion expired or not yet valid")
	ErrReplay    = errors.New("assertion nonce already used")
	ErrNoKeys    = errors.New("no hub public key configured")
)

var (
	deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	noncePattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{22,128}$`)
)

// Claims is the signed payload.
type Claims struct {
	Typ        string `json:"typ"`
	Aud        string `json:"aud"`
	Leg        string `json:"leg"`
	Method     string `json:"htm"`
	Path       string `json:"htp"`
	Sub        string `json:"sub"`
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	IssuedAt   int64  `json:"iat"`
	Expires    int64  `json:"exp"`
	Nonce      string `json:"nonce"`
}

// Expect is what a verifier requires of the claims.
type Expect struct {
	Audience string
	Leg      string
	Method   string
	Path     string
}

// NewNonce returns 16 random bytes, base64url encoded without padding.
func NewNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Sign encodes and signs c. It fills Typ, and Nonce when empty.
func Sign(key ed25519.PrivateKey, c Claims) (string, error) {
	c.Typ = Type
	if c.Nonce == "" {
		c.Nonce = NewNonce()
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signed := Prefix + base64.RawURLEncoding.EncodeToString(payload)
	return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(signed))), nil
}

// Verifier checks assertions against a set of public keys and remembers
// nonces until they expire. It is safe for concurrent use.
type Verifier struct {
	Keys []ed25519.PublicKey
	Now  func() time.Time

	mu     sync.Mutex
	nonces map[string]time.Time
}

// NewVerifier returns a verifier for keys.
func NewVerifier(keys []ed25519.PublicKey) *Verifier {
	return &Verifier{Keys: keys, Now: time.Now, nonces: map[string]time.Time{}}
}

// Verify checks encoded and returns its claims. A nonce is consumed only
// when every other check passes.
func (v *Verifier) Verify(encoded string, want Expect) (*Claims, error) {
	if len(v.Keys) == 0 {
		return nil, ErrNoKeys
	}
	if len(encoded) > MaxEncodedLen || !strings.HasPrefix(encoded, Prefix) {
		return nil, ErrMalformed
	}
	signed, sigText, ok := strings.Cut(strings.TrimPrefix(encoded, Prefix), ".")
	if !ok || strings.Contains(sigText, ".") {
		return nil, ErrMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(signed)
	if err != nil {
		return nil, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigText)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrMalformed
	}
	msg := []byte(Prefix + signed)
	good := false
	for _, k := range v.Keys {
		if ed25519.Verify(k, msg, sig) {
			good = true
			break
		}
	}
	if !good {
		return nil, ErrSignature
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, ErrMalformed
	}
	if err := checkClaims(&c, want); err != nil {
		return nil, err
	}
	now := v.now()
	iat, exp := time.Unix(c.IssuedAt, 0), time.Unix(c.Expires, 0)
	if exp.Before(iat) || exp.Sub(iat) > MaxLifetime || iat.After(now.Add(ClockSkew)) || now.After(exp.Add(ClockSkew)) {
		return nil, ErrExpired
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.nonces == nil {
		v.nonces = map[string]time.Time{}
	}
	for n, until := range v.nonces {
		if now.After(until) {
			delete(v.nonces, n)
		}
	}
	if _, seen := v.nonces[c.Nonce]; seen {
		return nil, ErrReplay
	}
	v.nonces[c.Nonce] = exp.Add(ClockSkew)
	return &c, nil
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func checkClaims(c *Claims, want Expect) error {
	switch {
	case c.Typ != Type:
		return fmt.Errorf("%w: typ", ErrClaims)
	case want.Audience == "" || c.Aud != want.Audience:
		return fmt.Errorf("%w: aud", ErrClaims)
	case c.Leg != want.Leg:
		return fmt.Errorf("%w: leg", ErrClaims)
	case c.Method != want.Method:
		return fmt.Errorf("%w: htm", ErrClaims)
	case c.Path != want.Path:
		return fmt.Errorf("%w: htp", ErrClaims)
	case c.Sub == "" || len(c.Sub) > 256:
		return fmt.Errorf("%w: sub", ErrClaims)
	case !deviceIDPattern.MatchString(c.DeviceID):
		return fmt.Errorf("%w: device_id", ErrClaims)
	case c.DeviceName == "" || len(c.DeviceName) > 64:
		return fmt.Errorf("%w: device_name", ErrClaims)
	case !noncePattern.MatchString(c.Nonce):
		return fmt.Errorf("%w: nonce", ErrClaims)
	}
	return nil
}

// ParsePublicKeys reads one or more Ed25519 public keys: PEM blocks of type
// PUBLIC KEY (SPKI), or lines holding the 32 raw key bytes in base64
// (standard or URL alphabet, padding optional). Blank lines and lines
// starting with # are ignored.
func ParsePublicKeys(text string) ([]ed25519.PublicKey, error) {
	var keys []ed25519.PublicKey
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "-----BEGIN "):
			j := i
			for j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "-----END ") {
				j++
			}
			if j == len(lines) {
				return nil, fmt.Errorf("%w: unterminated PEM block", ErrMalformed)
			}
			k, err := parsePEMKey(strings.Join(lines[i:j+1], "\n"))
			if err != nil {
				return nil, err
			}
			keys = append(keys, k)
			i = j
		default:
			raw, err := decodeAnyBase64(line)
			if err != nil || len(raw) != ed25519.PublicKeySize {
				return nil, fmt.Errorf("%w: key line is not 32 bytes of base64", ErrMalformed)
			}
			keys = append(keys, ed25519.PublicKey(raw))
		}
	}
	if len(keys) == 0 {
		return nil, ErrNoKeys
	}
	return keys, nil
}

func parsePEMKey(text string) (ed25519.PublicKey, error) {
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, fmt.Errorf("%w: bad PEM block", ErrMalformed)
	}
	if block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("%w: PEM block %q", ErrMalformed, block.Type)
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	k, ok := pub.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: not an Ed25519 key", ErrMalformed)
	}
	return k, nil
}

func decodeAnyBase64(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	if strings.ContainsAny(s, "-_") {
		return base64.RawURLEncoding.DecodeString(s)
	}
	return base64.RawStdEncoding.DecodeString(s)
}
