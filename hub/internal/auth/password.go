// Package auth holds the parts of the hub's login that do not depend on
// the web server: password hashes, login limits, the client's address
// behind proxies, the identity a reverse proxy sends and account names.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Password length limits, in characters and bytes.
const (
	MinPassword      = 12
	MaxPasswordBytes = 1024
)

// Password errors.
var (
	ErrWeakPassword = fmt.Errorf("a password needs at least %d characters", MinPassword)
	ErrLongPassword = fmt.Errorf("a password may have at most %d bytes", MaxPasswordBytes)
	ErrBadHash      = errors.New("auth: not an argon2id hash")
)

// CheckPassword checks a new password against the length rules and the
// account's name.
func CheckPassword(password, name string) error {
	if len(password) > MaxPasswordBytes {
		return ErrLongPassword
	}
	if utf8.RuneCountInString(password) < MinPassword || strings.EqualFold(password, name) {
		return ErrWeakPassword
	}
	return nil
}

// Params are argon2id's costs.
type Params struct {
	// Memory in KiB.
	Memory  uint32
	Time    uint32
	Threads uint8
}

// DefaultParams follow OWASP's argon2id baseline: 19 MiB, 2 passes, one
// thread.
func DefaultParams() Params { return Params{Memory: 19 * 1024, Time: 2, Threads: 1} }

const (
	saltLen = 16
	keyLen  = 32
)

// Hasher hashes and checks passwords with argon2id. It runs at most a few
// at once, so a burst of logins cannot use up the server's memory.
type Hasher struct {
	params Params
	slots  chan struct{}
	dummy  string
}

// NewHasher returns a Hasher that runs at most concurrent hashes at once.
func NewHasher(p Params, concurrent int) *Hasher {
	if concurrent < 1 {
		concurrent = 1
	}
	h := &Hasher{params: p, slots: make(chan struct{}, concurrent)}
	h.dummy = h.hash("dummy password for unknown accounts", make([]byte, saltLen))
	return h
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.slots }

func (h *Hasher) hash(password string, salt []byte) string {
	p := h.params
	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.Memory, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// Hash returns the PHC string of password with a new salt.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	return h.hash(password, salt), nil
}

// Verify reports whether password matches encoded. An empty encoded (an
// account with no password) is checked against a dummy hash, so it takes
// as long as a real one and never matches.
func (h *Hasher) Verify(ctx context.Context, encoded, password string) (bool, error) {
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()
	if encoded == "" {
		_, _ = verify(h.dummy, password)
		return false, nil
	}
	return verify(encoded, password)
}

func verify(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrBadHash
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil ||
		p.Memory == 0 || p.Memory > 1<<21 || p.Time == 0 || p.Time > 16 || p.Threads == 0 {
		return false, ErrBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 || len(want) > 128 {
		return false, ErrBadHash
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(min(uint64(len(want)), math.MaxUint32)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
