// Package secretbox keeps the hub's own secrets (the Telegram bot token set
// in Settings) in its database, sealed with a key derived from the hub's
// session secret, which lives with the install's other secrets and not in
// the database (unless the install sets none). A secret sealed under
// another session secret cannot be opened: it reads as unset.
package secretbox

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// Store keeps settings as text.
type Store interface {
	Setting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
	DeleteSetting(ctx context.Context, key string) error
}

// Box seals and opens secrets kept in a Store.
type Box struct {
	st   Store
	aead cipher.AEAD
}

// ErrSealedElsewhere is a secret sealed under another session secret.
var ErrSealedElsewhere = errors.New("secretbox: sealed with another key")

const prefix = "v1:"

// New returns a Box whose key is derived from secret.
func New(st Store, secret string) (*Box, error) {
	if len(secret) < 16 {
		return nil, errors.New("secretbox: the session secret is too short")
	}
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, "talaria hub secret store", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{st: st, aead: aead}, nil
}

func (b *Box) seal(name, value string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(value), []byte(name))
	return prefix + base64.RawStdEncoding.EncodeToString(out), nil
}

func (b *Box) open(name, sealed string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if err != nil || !strings.HasPrefix(sealed, prefix) || len(raw) < b.aead.NonceSize() {
		return "", ErrSealedElsewhere
	}
	n := b.aead.NonceSize()
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], []byte(name))
	if err != nil {
		return "", ErrSealedElsewhere
	}
	return string(plain), nil
}

// Put seals value under name.
func (b *Box) Put(ctx context.Context, name, value string) error {
	sealed, err := b.seal(name, value)
	if err != nil {
		return err
	}
	return b.st.SetSetting(ctx, "secret:"+name, sealed)
}

// Get opens the secret name; "" and false when unset. A secret this box
// cannot open comes back with ErrSealedElsewhere.
func (b *Box) Get(ctx context.Context, name string) (string, bool, error) {
	sealed, ok, err := b.st.Setting(ctx, "secret:"+name)
	if err != nil || !ok {
		return "", false, err
	}
	v, err := b.open(name, sealed)
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// Delete forgets the secret name.
func (b *Box) Delete(ctx context.Context, name string) error {
	return b.st.DeleteSetting(ctx, "secret:"+name)
}
