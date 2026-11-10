package config

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"time"
)

// Defaults of the devices durations, also for a zero value.
const (
	defaultPairingTTL      = 10 * time.Minute
	defaultPollInterval    = 5 * time.Second
	defaultRecheckInterval = 10 * time.Second
)

// PrivateKey reads the signing key from SigningKeyFile, else SigningKey.
func (d Device) PrivateKey() (ed25519.PrivateKey, error) {
	text := d.SigningKey
	if d.SigningKeyFile != "" {
		b, err := os.ReadFile(d.SigningKeyFile)
		if err != nil {
			return nil, fmt.Errorf("%w: device.signing_key_file: %w", ErrDevice, err)
		}
		text = string(b)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("%w: device.enabled needs device.signing_key or device.signing_key_file", ErrDevice)
	}
	key, err := ParseSigningKey(text)
	if err != nil {
		return nil, fmt.Errorf("%w: device.signing_key: %w", ErrDevice, err)
	}
	return key, nil
}

// ParseSigningKey reads an Ed25519 private key: a PEM PRIVATE KEY block
// (PKCS #8), or base64 (standard or URL alphabet, padding optional) of the
// 32-byte seed or the 64-byte seed and public key.
func ParseSigningKey(text string) (ed25519.PrivateKey, error) {
	if strings.HasPrefix(text, "-----BEGIN ") {
		block, _ := pem.Decode([]byte(text))
		if block == nil || block.Type != "PRIVATE KEY" {
			return nil, fmt.Errorf("not a PEM PRIVATE KEY block")
		}
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		key, ok := k.(ed25519.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("not an Ed25519 key")
		}
		return key, nil
	}
	s := strings.TrimRight(text, "=")
	enc := base64.RawStdEncoding
	if strings.ContainsAny(s, "-_") {
		enc = base64.RawURLEncoding
	}
	raw, err := enc.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("not PEM or base64")
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		key := ed25519.NewKeyFromSeed(raw[:ed25519.SeedSize])
		if !key.Equal(ed25519.PrivateKey(raw)) {
			return nil, fmt.Errorf("the public half does not match the seed")
		}
		return key, nil
	}
	return nil, fmt.Errorf("base64 key must hold 32 or 64 bytes")
}

// Proxies parses TrustedProxies.
func (d Device) Proxies() ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(d.TrustedProxies))
	for _, s := range d.TrustedProxies {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			a, aerr := netip.ParseAddr(s)
			if aerr != nil {
				return nil, fmt.Errorf("%w: device.trusted_proxies %q: %w", ErrDevice, s, err)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

var serviceDeviceID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validateServiceDevices(i int, b *Backend, seen map[string]bool) error {
	for j := range b.ServiceDevices {
		d := &b.ServiceDevices[j]
		if !serviceDeviceID.MatchString(d.ID) || strings.HasPrefix(d.ID, "d_") || seen[d.ID] {
			return fmt.Errorf("%w: backends[%d].service_devices[%d].id %q must be unique letters, digits, _ and -, not starting with d_",
				ErrBackend, i, j, d.ID)
		}
		seen[d.ID] = true
		if !strings.HasPrefix(d.Token, "hdt_") || len(d.Token) < 36 {
			return fmt.Errorf("%w: backends[%d].service_devices[%d].token must be hdt_ and at least 32 more characters", ErrBackend, i, j)
		}
		if d.Name == "" {
			d.Name = d.ID
		}
		if len(d.Name) > 64 {
			return fmt.Errorf("%w: backends[%d].service_devices[%d].name is longer than 64 bytes", ErrBackend, i, j)
		}
	}
	return nil
}
