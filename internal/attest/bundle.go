package attest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Limits on what a bundle may hold.
const (
	maxTlogEntries = 8
	maxProofHashes = 64
	maxSubjects    = 4096
)

const (
	jsonNull          = "null"
	payloadTypeInToto = "application/vnd.in-toto+json"
	statementV1       = "https://in-toto.io/Statement/v1"
)

// sigstoreBundle is the protobuf-JSON form of a Sigstore bundle
// (dev.sigstore.bundle.v1.Bundle), as GitHub returns it.
type sigstoreBundle struct {
	MediaType            string `json:"mediaType"`
	VerificationMaterial *struct {
		Certificate *struct {
			RawBytes b64 `json:"rawBytes"`
		} `json:"certificate"`
		X509CertificateChain *struct {
			Certificates []struct {
				RawBytes b64 `json:"rawBytes"`
			} `json:"certificates"`
		} `json:"x509CertificateChain"`
		PublicKey   json.RawMessage `json:"publicKey"`
		TlogEntries []tlogEntry     `json:"tlogEntries"`
	} `json:"verificationMaterial"`
	DSSEEnvelope     *dsseEnvelope   `json:"dsseEnvelope"`
	MessageSignature json.RawMessage `json:"messageSignature"`
}

type dsseEnvelope struct {
	Payload     b64    `json:"payload"`
	PayloadType string `json:"payloadType"`
	Signatures  []struct {
		Sig b64 `json:"sig"`
	} `json:"signatures"`
}

type tlogEntry struct {
	LogIndex *int64s `json:"logIndex"`
	LogID    struct {
		KeyID b64 `json:"keyId"`
	} `json:"logId"`
	KindVersion struct {
		Kind    string `json:"kind"`
		Version string `json:"version"`
	} `json:"kindVersion"`
	IntegratedTime   int64s `json:"integratedTime"`
	InclusionPromise *struct {
		SignedEntryTimestamp b64 `json:"signedEntryTimestamp"`
	} `json:"inclusionPromise"`
	InclusionProof *struct {
		LogIndex   *int64s `json:"logIndex"`
		RootHash   b64     `json:"rootHash"`
		TreeSize   int64s  `json:"treeSize"`
		Hashes     []b64   `json:"hashes"`
		Checkpoint *struct {
			Envelope string `json:"envelope"`
		} `json:"checkpoint"`
	} `json:"inclusionProof"`
	CanonicalizedBody b64 `json:"canonicalizedBody"`
}

// bundleVersion is the minor version of a bundle media type, or -1.
func bundleVersion(mediaType string) int {
	switch mediaType {
	case "application/vnd.dev.sigstore.bundle+json;version=0.1":
		return 1
	case "application/vnd.dev.sigstore.bundle+json;version=0.2":
		return 2
	case "application/vnd.dev.sigstore.bundle+json;version=0.3", "application/vnd.dev.sigstore.bundle.v0.3+json":
		return 3
	}
	return -1
}

// verified is what a bundle that passed verification says.
type verified struct {
	cert      *x509.Certificate
	ext       fulcioExtensions
	san       string
	statement statement
}

type statement struct {
	Type          string `json:"_type"`
	PredicateType string `json:"predicateType"`
	Subject       []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
}

// verifyBundle checks the signatures of a Sigstore bundle against tr: the
// DSSE signature with the certificate's key, the certificate chain to
// Fulcio, a signed certificate timestamp, and a Rekor entry for this
// envelope and certificate with a signed entry timestamp, whose integrated
// time falls in the certificate's validity. It checks no policy.
func (tr *TrustedRoot) verifyBundle(raw []byte, now time.Time) (*verified, error) {
	b, version, leaf, err := parseBundle(raw)
	if err != nil {
		return nil, err
	}
	vm := b.VerificationMaterial
	env := b.DSSEEnvelope
	if env.PayloadType != payloadTypeInToto {
		return nil, fmt.Errorf("DSSE payload type %q, want %q", env.PayloadType, payloadTypeInToto)
	}
	if len(env.Signatures) != 1 || len(env.Signatures[0].Sig) == 0 {
		return nil, fmt.Errorf("DSSE envelope has %d signatures, want 1", len(env.Signatures))
	}
	sig := env.Signatures[0].Sig

	entries := vm.TlogEntries
	if len(entries) == 0 {
		return nil, errors.New("bundle has no transparency log entry")
	}
	if len(entries) > maxTlogEntries {
		return nil, fmt.Errorf("bundle has %d transparency log entries", len(entries))
	}
	var integrated time.Time
	var errs []error
	for i := range entries {
		t, err := tr.verifyTlogEntry(&entries[i], version, env, sig, leaf)
		if err != nil {
			errs = append(errs, fmt.Errorf("transparency log entry %d: %w", i+1, err))
			continue
		}
		integrated = t
		break
	}
	if integrated.IsZero() {
		return nil, errors.Join(errs...)
	}
	if integrated.After(now.Add(5 * time.Minute)) {
		return nil, fmt.Errorf("the transparency log integrated time %s is in the future", integrated.UTC().Format(time.RFC3339))
	}
	issuer, err := tr.verifyChain(leaf, integrated)
	if err != nil {
		return nil, err
	}
	if err := tr.verifySCT(leaf, issuer); err != nil {
		return nil, err
	}
	if err := verifyDSSE(leaf, env, sig); err != nil {
		return nil, err
	}
	ext, err := parseFulcioExtensions(leaf)
	if err != nil {
		return nil, err
	}
	san, err := subjectURI(leaf)
	if err != nil {
		return nil, err
	}
	if err := checkJSON(env.Payload); err != nil {
		return nil, fmt.Errorf("statement: %w", err)
	}
	var st statement
	if err := json.Unmarshal(env.Payload, &st); err != nil {
		return nil, fmt.Errorf("statement: %w", err)
	}
	if st.Type != statementV1 {
		return nil, fmt.Errorf("statement type %q, want %q", st.Type, statementV1)
	}
	if len(st.Subject) > maxSubjects {
		return nil, fmt.Errorf("statement has %d subjects", len(st.Subject))
	}
	return &verified{cert: leaf, ext: ext, san: san, statement: st}, nil
}

// parseBundle reads a bundle that holds a DSSE envelope signed with a
// certificate, and returns it with its minor version and the certificate.
func parseBundle(raw []byte) (*sigstoreBundle, int, *x509.Certificate, error) {
	if err := checkJSON(raw); err != nil {
		return nil, 0, nil, fmt.Errorf("bundle: %w", err)
	}
	var b sigstoreBundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, 0, nil, fmt.Errorf("bundle: %w", err)
	}
	version := bundleVersion(b.MediaType)
	if version < 0 {
		return nil, 0, nil, fmt.Errorf("bundle media type %q is not supported", b.MediaType)
	}
	vm := b.VerificationMaterial
	if vm == nil {
		return nil, 0, nil, errors.New("bundle has no verification material")
	}
	if b.DSSEEnvelope == nil || len(b.MessageSignature) > 0 && string(b.MessageSignature) != jsonNull {
		return nil, 0, nil, errors.New("bundle must hold a DSSE envelope and no message signature")
	}
	var certDER []byte
	switch {
	case len(vm.PublicKey) > 0 && string(vm.PublicKey) != jsonNull:
		return nil, 0, nil, errors.New("bundle is signed with a public key, not a Fulcio certificate")
	case vm.Certificate != nil && vm.X509CertificateChain != nil:
		return nil, 0, nil, errors.New("bundle has both a certificate and a certificate chain")
	case vm.Certificate != nil:
		certDER = vm.Certificate.RawBytes
	case vm.X509CertificateChain != nil && version >= 3:
		return nil, 0, nil, errors.New("a v0.3 bundle must hold its certificate alone, not a chain")
	case vm.X509CertificateChain != nil && len(vm.X509CertificateChain.Certificates) > 0:
		certDER = vm.X509CertificateChain.Certificates[0].RawBytes
	}
	if len(certDER) == 0 {
		return nil, 0, nil, errors.New("bundle has no signing certificate")
	}
	leaf, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("signing certificate: %w", err)
	}
	return &b, version, leaf, nil
}

// pae is the DSSE v1 pre-authentication encoding.
func pae(payloadType string, payload []byte) []byte {
	var b bytes.Buffer
	b.WriteString("DSSEv1 ")
	b.WriteString(strconv.Itoa(len(payloadType)))
	b.WriteByte(' ')
	b.WriteString(payloadType)
	b.WriteByte(' ')
	b.WriteString(strconv.Itoa(len(payload)))
	b.WriteByte(' ')
	b.Write(payload)
	return b.Bytes()
}

// verifyDSSE checks the envelope signature with the certificate's key.
func verifyDSSE(leaf *x509.Certificate, env *dsseEnvelope, sig []byte) error {
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("signing certificate key type %T, want ECDSA", leaf.PublicKey)
	}
	msg := pae(env.PayloadType, env.Payload)
	var digest []byte
	switch pub.Curve {
	case elliptic.P256():
		s := sha256.Sum256(msg)
		digest = s[:]
	case elliptic.P384():
		s := sha512.Sum384(msg)
		digest = s[:]
	default:
		return fmt.Errorf("signing certificate curve %s is not supported", pub.Curve.Params().Name)
	}
	if !ecdsa.VerifyASN1(pub, digest, sig) {
		return errors.New("the DSSE signature does not verify with the signing certificate")
	}
	return nil
}

type hashJSON struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

// dsseBody is a Rekor dsse v0.0.1 entry.
type dsseBody struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		PayloadHash *hashJSON `json:"payloadHash"`
		Signatures  []struct {
			Signature string `json:"signature"`
			Verifier  b64    `json:"verifier"`
		} `json:"signatures"`
	} `json:"spec"`
}

// intotoBody is a Rekor intoto v0.0.2 entry.
type intotoBody struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Content struct {
			Envelope struct {
				PayloadType string `json:"payloadType"`
				Signatures  []struct {
					Sig       b64 `json:"sig"`
					PublicKey b64 `json:"publicKey"`
				} `json:"signatures"`
			} `json:"envelope"`
			PayloadHash *hashJSON `json:"payloadHash"`
		} `json:"content"`
	} `json:"spec"`
}

// verifyTlogEntry checks a Rekor v1 entry: its log is trusted, its signed
// entry timestamp verifies, its body records this envelope's payload hash,
// signature and certificate, and its inclusion proof and checkpoint, when
// present, verify. It returns the integrated time.
func (tr *TrustedRoot) verifyTlogEntry(e *tlogEntry, version int, env *dsseEnvelope, sig []byte, leaf *x509.Certificate) (time.Time, error) {
	logID := hex.EncodeToString(e.LogID.KeyID)
	key, ok := tr.tlogs[logID]
	switch {
	case !ok:
		return time.Time{}, fmt.Errorf("%w: transparency log %s is not in the trusted root", ErrUntrustedRoot, logID)
	case key.ecdsa == nil:
		return time.Time{}, fmt.Errorf("%w: transparency log %s is a Rekor v2 log, which this binary cannot verify", ErrUntrustedRoot, logID)
	}
	if e.LogIndex == nil || *e.LogIndex < 0 || e.IntegratedTime <= 0 {
		return time.Time{}, errors.New("no log index or integrated time")
	}
	integrated := time.Unix(int64(e.IntegratedTime), 0)
	if !key.validAt(integrated) {
		return time.Time{}, fmt.Errorf("transparency log key was not valid at %s", integrated.UTC())
	}
	if e.InclusionPromise == nil || len(e.InclusionPromise.SignedEntryTimestamp) == 0 {
		return time.Time{}, errors.New("no signed entry timestamp")
	}
	if len(e.CanonicalizedBody) == 0 {
		return time.Time{}, errors.New("no entry body")
	}
	payload := fmt.Sprintf(`{"body":%q,"integratedTime":%d,"logID":%q,"logIndex":%d}`,
		base64.StdEncoding.EncodeToString(e.CanonicalizedBody), int64(e.IntegratedTime), logID, int64(*e.LogIndex))
	sum := sha256.Sum256([]byte(payload))
	if !ecdsa.VerifyASN1(key.ecdsa, sum[:], e.InclusionPromise.SignedEntryTimestamp) {
		return time.Time{}, errors.New("the signed entry timestamp does not verify")
	}
	if err := checkBody(e, env, sig, leaf); err != nil {
		return time.Time{}, err
	}
	if e.InclusionProof == nil {
		if version >= 2 {
			return time.Time{}, errors.New("no inclusion proof")
		}
		return integrated, nil
	}
	if err := verifyInclusion(e, key); err != nil {
		return time.Time{}, err
	}
	return integrated, nil
}

// checkBody checks that the entry body records this envelope: the SHA-256
// of its payload, its one signature and the signing certificate.
func checkBody(e *tlogEntry, env *dsseEnvelope, sig []byte, leaf *x509.Certificate) error {
	if err := checkJSON(e.CanonicalizedBody); err != nil {
		return fmt.Errorf("entry body: %w", err)
	}
	var head struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := json.Unmarshal(e.CanonicalizedBody, &head); err != nil {
		return fmt.Errorf("entry body: %w", err)
	}
	if head.Kind != e.KindVersion.Kind || head.APIVersion != e.KindVersion.Version {
		return fmt.Errorf("entry body is %s %s, the bundle says %s %s", head.Kind, head.APIVersion, e.KindVersion.Kind, e.KindVersion.Version)
	}
	var payloadHash *hashJSON
	var entrySig, verifierPEM []byte
	switch {
	case head.Kind == "dsse" && head.APIVersion == "0.0.1":
		var body dsseBody
		if err := json.Unmarshal(e.CanonicalizedBody, &body); err != nil {
			return fmt.Errorf("entry body: %w", err)
		}
		if len(body.Spec.Signatures) != 1 {
			return fmt.Errorf("entry body has %d signatures, want 1", len(body.Spec.Signatures))
		}
		s, err := base64.StdEncoding.Strict().DecodeString(body.Spec.Signatures[0].Signature)
		if err != nil {
			return fmt.Errorf("entry body signature: %w", err)
		}
		payloadHash, entrySig, verifierPEM = body.Spec.PayloadHash, s, body.Spec.Signatures[0].Verifier
	case head.Kind == "intoto" && head.APIVersion == "0.0.2":
		var body intotoBody
		if err := json.Unmarshal(e.CanonicalizedBody, &body); err != nil {
			return fmt.Errorf("entry body: %w", err)
		}
		c := body.Spec.Content
		if c.Envelope.PayloadType != env.PayloadType {
			return fmt.Errorf("entry body payload type %q does not match the envelope", c.Envelope.PayloadType)
		}
		if len(c.Envelope.Signatures) != 1 {
			return fmt.Errorf("entry body has %d signatures, want 1", len(c.Envelope.Signatures))
		}
		s, err := base64.StdEncoding.Strict().DecodeString(string(c.Envelope.Signatures[0].Sig))
		if err != nil {
			return fmt.Errorf("entry body signature: %w", err)
		}
		payloadHash, entrySig, verifierPEM = c.PayloadHash, s, c.Envelope.Signatures[0].PublicKey
	default:
		return fmt.Errorf("entry kind %s %s is not supported", head.Kind, head.APIVersion)
	}
	want := sha256.Sum256(env.Payload)
	if payloadHash == nil || payloadHash.Algorithm != "sha256" || !strings.EqualFold(payloadHash.Value, hex.EncodeToString(want[:])) {
		return errors.New("entry body payload hash does not match the envelope payload")
	}
	if !bytes.Equal(entrySig, sig) {
		return errors.New("entry body signature does not match the envelope signature")
	}
	block, rest := pem.Decode(verifierPEM)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 || !bytes.Equal(block.Bytes, leaf.Raw) {
		return errors.New("entry body certificate does not match the signing certificate")
	}
	return nil
}

// verifyInclusion checks the RFC 6962 inclusion proof of the entry body
// and that its checkpoint is signed by the log key and commits to the
// proof's root hash and tree size.
func verifyInclusion(e *tlogEntry, key logKey) error {
	p := e.InclusionProof
	if p.LogIndex == nil || *p.LogIndex < 0 || p.TreeSize <= 0 || int64(*p.LogIndex) >= int64(p.TreeSize) || len(p.Hashes) > maxProofHashes {
		return errors.New("malformed inclusion proof")
	}
	index, size := uint64(*p.LogIndex), uint64(p.TreeSize)
	leaf := sha256.Sum256(append([]byte{0}, e.CanonicalizedBody...))
	proof := make([][]byte, len(p.Hashes))
	for i, h := range p.Hashes {
		if len(h) != sha256.Size {
			return errors.New("malformed inclusion proof hash")
		}
		proof[i] = h
	}
	root, err := rootFromInclusionProof(index, size, leaf[:], proof)
	if err != nil {
		return err
	}
	if !bytes.Equal(root, p.RootHash) {
		return errors.New("the inclusion proof does not lead to its root hash")
	}
	if p.Checkpoint == nil || p.Checkpoint.Envelope == "" {
		return errors.New("the inclusion proof has no checkpoint")
	}
	return verifyCheckpoint(p.Checkpoint.Envelope, key, size, p.RootHash)
}

func hashChildren(l, r []byte) []byte {
	b := make([]byte, 0, 1+len(l)+len(r))
	b = append(b, 1)
	b = append(b, l...)
	b = append(b, r...)
	s := sha256.Sum256(b)
	return s[:]
}

// rootFromInclusionProof computes the root of a tree of size leaves from
// the hash of leaf index and its inclusion proof (RFC 9162 2.1.3.2).
func rootFromInclusionProof(index, size uint64, leaf []byte, proof [][]byte) ([]byte, error) {
	fn, sn := index, size-1
	r := leaf
	for _, p := range proof {
		if sn == 0 {
			return nil, errors.New("the inclusion proof is too long")
		}
		if fn&1 == 1 || fn == sn {
			r = hashChildren(p, r)
			if fn&1 == 0 {
				for fn&1 == 0 && fn != 0 {
					fn >>= 1
					sn >>= 1
				}
			}
		} else {
			r = hashChildren(r, p)
		}
		fn >>= 1
		sn >>= 1
	}
	if sn != 0 {
		return nil, errors.New("the inclusion proof is too short")
	}
	return r, nil
}

// verifyCheckpoint checks a signed note: one signature line of the log
// key verifies over the note text, and the note commits to the tree of
// the given size and root hash.
func verifyCheckpoint(envelope string, key logKey, size uint64, root []byte) error {
	split := strings.LastIndex(envelope, "\n\n")
	if split < 0 || !strings.HasSuffix(envelope, "\n") {
		return errors.New("malformed checkpoint")
	}
	text, sigs := envelope[:split+1], envelope[split+2:]
	lines := strings.Split(text, "\n")
	if len(lines) < 4 || lines[0] == "" {
		return errors.New("malformed checkpoint")
	}
	n, err := strconv.ParseUint(lines[1], 10, 64)
	if err != nil || n != size {
		return fmt.Errorf("the checkpoint tree size %q is not the proof's %d", lines[1], size)
	}
	h, err := base64.StdEncoding.Strict().DecodeString(lines[2])
	if err != nil || !bytes.Equal(h, root) {
		return errors.New("the checkpoint root hash is not the proof's")
	}
	keyHash := sha256.Sum256(key.der)
	digest := sha256.Sum256([]byte(text))
	for line := range strings.SplitSeq(strings.TrimSuffix(sigs, "\n"), "\n") {
		rest, ok := strings.CutPrefix(line, "— ")
		if !ok {
			return errors.New("malformed checkpoint signature line")
		}
		_, b, ok := strings.Cut(rest, " ")
		if !ok {
			return errors.New("malformed checkpoint signature line")
		}
		s, err := base64.StdEncoding.Strict().DecodeString(b)
		if err != nil || len(s) < 5 {
			return errors.New("malformed checkpoint signature")
		}
		if bytes.Equal(s[:4], keyHash[:4]) && ecdsa.VerifyASN1(key.ecdsa, digest[:], s[4:]) {
			return nil
		}
	}
	return errors.New("no checkpoint signature verifies with the log key")
}
