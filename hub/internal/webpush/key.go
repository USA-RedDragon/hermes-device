// Package webpush sends Web Push messages signed with VAPID (RFC 8292) and
// encrypted with aes128gcm (RFC 8291, RFC 8188), using only the standard library.
package webpush

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
)

// VAPID holds an application server's P-256 signing key.
type VAPID struct {
	priv *ecdsa.PrivateKey
	pub  []byte

	// allowAnyEndpoint skips ValidEndpoint in Send. Only tests set it.
	allowAnyEndpoint bool
}

// ParseKey reads a P-256 private key: a PEM block (PKCS#8 "PRIVATE KEY" or
// SEC1 "EC PRIVATE KEY"), or the raw 32-byte scalar in base64url (padded or
// not, as web-push libraries print it).
func ParseKey(s string) (*VAPID, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("webpush: empty VAPID key")
	}
	if strings.HasPrefix(s, "-----BEGIN") {
		return parsePEM([]byte(s))
	}
	raw, err := decodeB64URL(s)
	if err != nil {
		return nil, fmt.Errorf("webpush: VAPID key is neither PEM nor base64url: %w", err)
	}
	k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, fmt.Errorf("webpush: VAPID key: %w", err)
	}
	return newVAPID(k)
}

func parsePEM(data []byte) (*VAPID, error) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, errors.New("webpush: no private key PEM block found")
		}
		switch block.Type {
		case "EC PARAMETERS":
			continue
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("webpush: VAPID key: %w", err)
			}
			ek, ok := k.(*ecdsa.PrivateKey)
			if !ok {
				return nil, fmt.Errorf("webpush: VAPID key is %T, want ECDSA P-256", k)
			}
			return newVAPID(ek)
		case "EC PRIVATE KEY":
			k, err := x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, fmt.Errorf("webpush: VAPID key: %w", err)
			}
			return newVAPID(k)
		default:
			return nil, fmt.Errorf("webpush: unsupported PEM block %q", block.Type)
		}
	}
}

func newVAPID(k *ecdsa.PrivateKey) (*VAPID, error) {
	if k.Curve != elliptic.P256() {
		return nil, errors.New("webpush: VAPID key is not on P-256")
	}
	pub, err := k.PublicKey.Bytes()
	if err != nil {
		return nil, fmt.Errorf("webpush: VAPID public key: %w", err)
	}
	return &VAPID{priv: k, pub: pub}, nil
}

// GenerateKey returns a new P-256 key as a PKCS#8 PEM string, for tests and docs.
func GenerateKey() (string, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("webpush: generate key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return "", fmt.Errorf("webpush: marshal key: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// PublicKey is the uncompressed 65-byte public key in base64url without
// padding (the browser's applicationServerKey).
func (v *VAPID) PublicKey() string {
	return base64.RawURLEncoding.EncodeToString(v.pub)
}

func decodeB64URL(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(s), "="))
	if err != nil {
		return nil, fmt.Errorf("decode base64url: %w", err)
	}
	return b, nil
}
