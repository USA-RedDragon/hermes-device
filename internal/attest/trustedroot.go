package attest

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// publicGoodRootJSON is the trusted root of the Sigstore public-good
// instance: targets/trusted_root.json of github.com/sigstore/root-signing
// (TUF timestamp version 831), recorded on 2026-11-11. It is byte for byte
// what `gh attestation trusted-root` printed for the public-good instance
// that day, with SHA-256
// 6494e21ea73fa7ee769f85f57d5a3e6a08725eae1e38c755fc3517c9e6bc0b66.
//
//go:embed trusted_root.json
var publicGoodRootJSON []byte

// PublicGoodRootDate is the day publicGoodRootJSON was recorded.
const PublicGoodRootDate = "2026-11-11"

// TrustedRoot is the Sigstore trust material: Fulcio certificate
// authorities, Rekor transparency logs and certificate transparency logs.
type TrustedRoot struct {
	cas    []certificateAuthority
	tlogs  map[string]logKey
	ctlogs map[string]logKey
}

type certificateAuthority struct {
	root          *x509.Certificate
	intermediates []*x509.Certificate
	start, end    time.Time
}

type logKey struct {
	// ecdsa is set for Rekor v1 and CT logs. A key of another type (the
	// Ed25519 keys of Rekor v2) is known but cannot verify entries here.
	ecdsa      *ecdsa.PublicKey
	other      bool
	der        []byte
	start, end time.Time
}

func (k logKey) validAt(t time.Time) bool {
	return !t.Before(k.start) && (k.end.IsZero() || !t.After(k.end))
}

type validFor struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type trustedRootJSON struct {
	MediaType string `json:"mediaType"`
	Tlogs     []struct {
		PublicKey struct {
			RawBytes b64      `json:"rawBytes"`
			ValidFor validFor `json:"validFor"`
		} `json:"publicKey"`
		LogID struct {
			KeyID b64 `json:"keyId"`
		} `json:"logId"`
	} `json:"tlogs"`
	CertificateAuthorities []struct {
		CertChain struct {
			Certificates []struct {
				RawBytes b64 `json:"rawBytes"`
			} `json:"certificates"`
		} `json:"certChain"`
		ValidFor validFor `json:"validFor"`
	} `json:"certificateAuthorities"`
	Ctlogs []struct {
		PublicKey struct {
			RawBytes b64      `json:"rawBytes"`
			ValidFor validFor `json:"validFor"`
		} `json:"publicKey"`
		LogID struct {
			KeyID b64 `json:"keyId"`
		} `json:"logId"`
	} `json:"ctlogs"`
}

// PublicGoodRoot is the trusted root of the Sigstore public-good instance
// that is built into this binary.
func PublicGoodRoot() *TrustedRoot {
	tr, err := ParseTrustedRoot(publicGoodRootJSON)
	if err != nil {
		panic("attest: the embedded trusted root does not parse: " + err.Error())
	}
	return tr
}

// ParseTrustedRoot reads a Sigstore trusted_root.json
// (application/vnd.dev.sigstore.trustedroot+json;version=0.1). It keeps
// the parts that verify a bundle signed with a Fulcio certificate and
// logged in Rekor v1.
func ParseTrustedRoot(data []byte) (*TrustedRoot, error) {
	if err := checkJSON(data); err != nil {
		return nil, fmt.Errorf("trusted root: %w", err)
	}
	var j trustedRootJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("trusted root: %w", err)
	}
	if j.MediaType != "application/vnd.dev.sigstore.trustedroot+json;version=0.1" {
		return nil, fmt.Errorf("trusted root: media type %q", j.MediaType)
	}
	tr := &TrustedRoot{tlogs: map[string]logKey{}, ctlogs: map[string]logKey{}}
	for i, ca := range j.CertificateAuthorities {
		certs := ca.CertChain.Certificates
		if len(certs) == 0 {
			return nil, fmt.Errorf("trusted root: certificate authority %d has no certificates", i)
		}
		c := certificateAuthority{start: ca.ValidFor.Start, end: ca.ValidFor.End}
		for k, raw := range certs {
			cert, err := x509.ParseCertificate(raw.RawBytes)
			if err != nil {
				return nil, fmt.Errorf("trusted root: certificate authority %d: %w", i, err)
			}
			if !cert.IsCA {
				return nil, fmt.Errorf("trusted root: certificate authority %d: certificate %d is not a CA", i, k)
			}
			if k == len(certs)-1 {
				c.root = cert
			} else {
				c.intermediates = append(c.intermediates, cert)
			}
		}
		tr.cas = append(tr.cas, c)
	}
	add := func(m map[string]logKey, kind string, i int, raw, id []byte, vf validFor) error {
		pub, err := x509.ParsePKIXPublicKey(raw)
		if err != nil {
			return fmt.Errorf("trusted root: %s %d: %w", kind, i, err)
		}
		if vf.Start.IsZero() {
			return fmt.Errorf("trusted root: %s %d has no start of validity", kind, i)
		}
		k := logKey{der: raw, start: vf.Start, end: vf.End}
		switch pub := pub.(type) {
		case *ecdsa.PublicKey:
			if sum := sha256.Sum256(raw); string(sum[:]) != string(id) {
				return fmt.Errorf("trusted root: %s %d: the log ID is not the SHA-256 of the key", kind, i)
			}
			k.ecdsa = pub
		case ed25519.PublicKey:
			k.other = true
		default:
			return fmt.Errorf("trusted root: %s %d: key type %T", kind, i, pub)
		}
		m[hex.EncodeToString(id)] = k
		return nil
	}
	for i, l := range j.Tlogs {
		if err := add(tr.tlogs, "transparency log", i, l.PublicKey.RawBytes, l.LogID.KeyID, l.PublicKey.ValidFor); err != nil {
			return nil, err
		}
	}
	for i, l := range j.Ctlogs {
		if err := add(tr.ctlogs, "certificate transparency log", i, l.PublicKey.RawBytes, l.LogID.KeyID, l.PublicKey.ValidFor); err != nil {
			return nil, err
		}
	}
	if len(tr.cas) == 0 {
		return nil, errors.New("trusted root: no certificate authorities")
	}
	return tr, nil
}
