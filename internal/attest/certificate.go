package attest

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

//nolint:gochecknoglobals // fixed OIDs
var (
	oidFulcio         = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1}
	oidSCTList        = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11129, 2, 4, 2}
	oidSubjectAltName = asn1.ObjectIdentifier{2, 5, 29, 17}
)

// fulcioExtensions are the claims Fulcio copies from the OIDC token into
// the certificate, by the last arc of their OID under 1.3.6.1.4.1.57264.1.
type fulcioExtensions struct {
	Issuer                          string // 8 (1 is the old raw form)
	SourceRepositoryDigest          string // 13
	RunnerEnvironment               string // 11
	SourceRepositoryURI             string // 12
	SourceRepositoryRef             string // 14
	SourceRepositoryIdentifier      string // 15
	SourceRepositoryOwnerURI        string // 16
	SourceRepositoryOwnerIdentifier string // 17
	BuildConfigURI                  string // 18
	BuildTrigger                    string // 20
}

// parseFulcioExtensions reads the Fulcio extensions of cert. The
// extensions from arc 8 on are DER UTF8Strings; arcs 1 to 6 are the
// deprecated raw strings, which must agree with the DER ones when both are
// present.
func parseFulcioExtensions(cert *x509.Certificate) (fulcioExtensions, error) {
	var e fulcioExtensions
	raw := map[int]string{}
	der := map[int]string{}
	for _, ext := range cert.Extensions {
		if len(ext.Id) != len(oidFulcio)+1 || !ext.Id[:len(oidFulcio)].Equal(oidFulcio) {
			continue
		}
		arc := ext.Id[len(oidFulcio)]
		if arc <= 7 {
			if !utf8.Valid(ext.Value) {
				return e, fmt.Errorf("certificate extension %s is not UTF-8", ext.Id)
			}
			raw[arc] = string(ext.Value)
			continue
		}
		var v asn1.RawValue
		rest, err := asn1.Unmarshal(ext.Value, &v)
		if err != nil || len(rest) != 0 || v.Class != asn1.ClassUniversal || v.Tag != asn1.TagUTF8String || v.IsCompound || !utf8.Valid(v.Bytes) {
			return e, fmt.Errorf("certificate extension %s is not a DER UTF8String", ext.Id)
		}
		der[arc] = string(v.Bytes)
	}
	for old, cur := range map[int]int{1: 8, 2: 20, 6: 14} {
		if v, ok := raw[old]; ok && v != der[cur] {
			return e, fmt.Errorf("certificate extensions 1.3.6.1.4.1.57264.1.%d (%q) and .%d (%q) disagree", old, v, cur, der[cur])
		}
	}
	e.Issuer = der[8]
	e.RunnerEnvironment = der[11]
	e.SourceRepositoryURI = der[12]
	e.SourceRepositoryDigest = der[13]
	e.SourceRepositoryRef = der[14]
	e.SourceRepositoryIdentifier = der[15]
	e.SourceRepositoryOwnerURI = der[16]
	e.SourceRepositoryOwnerIdentifier = der[17]
	e.BuildConfigURI = der[18]
	e.BuildTrigger = der[20]
	return e, nil
}

// subjectURI returns the one subject alternative name of cert, which must
// be a URI, as the certificate encodes it.
func subjectURI(cert *x509.Certificate) (string, error) {
	var value []byte
	for _, ext := range cert.Extensions {
		if ext.Id.Equal(oidSubjectAltName) {
			value = ext.Value
		}
	}
	var seq asn1.RawValue
	rest, err := asn1.Unmarshal(value, &seq)
	if err != nil || len(rest) != 0 || seq.Class != asn1.ClassUniversal || seq.Tag != asn1.TagSequence {
		return "", errors.New("the certificate has no valid subject alternative name")
	}
	var names []asn1.RawValue
	for in := seq.Bytes; len(in) > 0; {
		var n asn1.RawValue
		if in, err = asn1.Unmarshal(in, &n); err != nil {
			return "", fmt.Errorf("subject alternative name: %w", err)
		}
		names = append(names, n)
	}
	if len(names) != 1 || names[0].Class != asn1.ClassContextSpecific || names[0].Tag != 6 || names[0].IsCompound {
		return "", fmt.Errorf("the certificate has %d subject alternative names, want one URI", len(names))
	}
	if !utf8.Valid(names[0].Bytes) {
		return "", errors.New("the certificate's subject alternative name is not text")
	}
	return string(names[0].Bytes), nil
}

// verifyChain checks that leaf chains to a Fulcio certificate authority of
// tr that was valid at t, for code signing, and returns the leaf's issuer.
func (tr *TrustedRoot) verifyChain(leaf *x509.Certificate, t time.Time) (*x509.Certificate, error) {
	if leaf.IsCA || leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return nil, errors.New("the signing certificate is a CA or may not sign")
	}
	if t.Before(leaf.NotBefore) || t.After(leaf.NotAfter) {
		return nil, fmt.Errorf("the signing certificate was not valid at the log's integrated time %s (valid %s to %s)",
			t.UTC().Format(time.RFC3339), leaf.NotBefore.UTC().Format(time.RFC3339), leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	var errs []error
	for _, ca := range tr.cas {
		if t.Before(ca.start) || !ca.end.IsZero() && t.After(ca.end) {
			continue
		}
		roots := x509.NewCertPool()
		roots.AddCert(ca.root)
		inter := x509.NewCertPool()
		for _, c := range ca.intermediates {
			inter.AddCert(c)
		}
		chains, err := leaf.Verify(x509.VerifyOptions{
			Roots: roots, Intermediates: inter, CurrentTime: t,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(chains) == 0 || len(chains[0]) < 2 {
			continue
		}
		return chains[0][1], nil
	}
	return nil, fmt.Errorf("%w: the signing certificate does not chain to a trusted Fulcio certificate authority: %w", ErrUntrustedRoot, errors.Join(errs...))
}

// verifySCT checks that at least one signed certificate timestamp embedded
// in leaf verifies with a CT log key of tr: the timestamp of a precertificate
// entry, which is leaf without the SCT extension, issued by issuer.
func (tr *TrustedRoot) verifySCT(leaf, issuer *x509.Certificate) error {
	var list []byte
	for _, ext := range leaf.Extensions {
		if ext.Id.Equal(oidSCTList) {
			rest, err := asn1.Unmarshal(ext.Value, &list)
			if err != nil || len(rest) != 0 {
				return errors.New("the SCT extension is not an OCTET STRING")
			}
		}
	}
	if list == nil {
		return errors.New("the signing certificate has no signed certificate timestamp")
	}
	scts, err := parseSCTList(list)
	if err != nil {
		return err
	}
	tbs, err := precertTBS(leaf.RawTBSCertificate)
	if err != nil {
		return err
	}
	if len(tbs) >= 1<<24 {
		return errors.New("the certificate is too large for an SCT")
	}
	keyHash := sha256.Sum256(issuer.RawSubjectPublicKeyInfo)
	var errs []error
	for i, s := range scts {
		key, ok := tr.ctlogs[fmt.Sprintf("%x", s.logID)]
		if !ok || key.ecdsa == nil {
			errs = append(errs, fmt.Errorf("SCT %d: log %x is not a trusted CT log", i+1, s.logID))
			continue
		}
		ts := time.UnixMilli(int64(s.timestamp)) //nolint:gosec // a time in ms
		if !key.validAt(ts) {
			errs = append(errs, fmt.Errorf("SCT %d: the CT log key was not valid at %s", i+1, ts.UTC()))
			continue
		}
		if s.hashAlg != 4 || s.sigAlg != 3 {
			errs = append(errs, fmt.Errorf("SCT %d: signature algorithm %d/%d, want ECDSA with SHA-256", i+1, s.hashAlg, s.sigAlg))
			continue
		}
		msg := make([]byte, 0, 50+len(tbs)+len(s.ext))
		msg = append(msg, 0, 0)
		msg = binary.BigEndian.AppendUint64(msg, s.timestamp)
		msg = append(msg, 0, 1)
		msg = append(msg, keyHash[:]...)
		msg = append(msg, byte(len(tbs)>>16), byte(len(tbs)>>8), byte(len(tbs))) //nolint:gosec // len(tbs) < 1<<24
		msg = append(msg, tbs...)
		msg = binary.BigEndian.AppendUint16(msg, uint16(len(s.ext))) //nolint:gosec // parsed from a uint16
		msg = append(msg, s.ext...)
		sum := sha256.Sum256(msg)
		if !ecdsa.VerifyASN1(key.ecdsa, sum[:], s.sig) {
			errs = append(errs, fmt.Errorf("SCT %d: the signature does not verify", i+1))
			continue
		}
		return nil
	}
	return fmt.Errorf("no signed certificate timestamp verifies: %w", errors.Join(errs...))
}

type sct struct {
	logID           []byte
	timestamp       uint64
	ext             []byte
	hashAlg, sigAlg byte
	sig             []byte
}

// tlsReader reads the TLS presentation language of RFC 6962.
type tlsReader struct {
	b   []byte
	err error
}

func (r *tlsReader) take(n int) []byte {
	if r.err != nil || n > len(r.b) {
		r.err = errors.New("truncated SCT data")
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *tlsReader) u8() byte {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (r *tlsReader) u16() int {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return int(binary.BigEndian.Uint16(b))
}

func parseSCTList(data []byte) ([]sct, error) {
	r := &tlsReader{b: data}
	list := &tlsReader{b: r.take(r.u16())}
	if r.err != nil || len(r.b) != 0 {
		return nil, errors.New("malformed SCT list")
	}
	var out []sct
	for len(list.b) > 0 && list.err == nil {
		s := &tlsReader{b: list.take(list.u16())}
		var v sct
		if s.u8() != 0 {
			return nil, errors.New("SCT version is not v1")
		}
		v.logID = s.take(32)
		if t := s.take(8); t != nil {
			v.timestamp = binary.BigEndian.Uint64(t)
		}
		v.ext = s.take(s.u16())
		v.hashAlg = s.u8()
		v.sigAlg = s.u8()
		v.sig = s.take(s.u16())
		if s.err != nil || len(s.b) != 0 {
			return nil, errors.New("malformed SCT")
		}
		out = append(out, v)
	}
	if list.err != nil || len(out) == 0 {
		return nil, errors.New("malformed SCT list")
	}
	return out, nil
}

// precertTBS is the TBSCertificate of the precertificate that the CT log
// signed: rawTBS with the SCT list extension removed.
func precertTBS(rawTBS []byte) ([]byte, error) {
	var tbs asn1.RawValue
	rest, err := asn1.Unmarshal(rawTBS, &tbs)
	if err != nil || len(rest) != 0 || tbs.Tag != asn1.TagSequence {
		return nil, errors.New("malformed TBSCertificate")
	}
	var out []byte
	removed := 0
	for in := tbs.Bytes; len(in) > 0; {
		var el asn1.RawValue
		if in, err = asn1.Unmarshal(in, &el); err != nil {
			return nil, fmt.Errorf("TBSCertificate: %w", err)
		}
		if el.Class != asn1.ClassContextSpecific || el.Tag != 3 {
			out = append(out, el.FullBytes...)
			continue
		}
		var exts asn1.RawValue
		if r, err := asn1.Unmarshal(el.Bytes, &exts); err != nil || len(r) != 0 || exts.Tag != asn1.TagSequence {
			return nil, errors.New("malformed certificate extensions")
		}
		var kept []byte
		for e := exts.Bytes; len(e) > 0; {
			var ext asn1.RawValue
			if e, err = asn1.Unmarshal(e, &ext); err != nil {
				return nil, fmt.Errorf("certificate extension: %w", err)
			}
			var id struct {
				ID       asn1.ObjectIdentifier
				Critical bool `asn1:"optional"`
				Value    []byte
			}
			if r, err := asn1.Unmarshal(ext.FullBytes, &id); err != nil || len(r) != 0 {
				return nil, errors.New("malformed certificate extension")
			}
			if id.ID.Equal(oidSCTList) {
				removed++
				continue
			}
			kept = append(kept, ext.FullBytes...)
		}
		seq, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: kept})
		if err != nil {
			return nil, err
		}
		wrapped, err := asn1.Marshal(asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 3, IsCompound: true, Bytes: seq})
		if err != nil {
			return nil, err
		}
		out = append(out, wrapped...)
	}
	if removed != 1 {
		return nil, fmt.Errorf("the certificate has %d SCT list extensions, want 1", removed)
	}
	return asn1.Marshal(asn1.RawValue{Class: asn1.ClassUniversal, Tag: asn1.TagSequence, IsCompound: true, Bytes: out})
}
