//nolint:goconst,forcetypeassert // test fixtures; a wrong assertion fails the test
package attest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"
)

const fileA = "a.tar.gz"

func TestCheckStatement(t *testing.T) {
	t.Parallel()
	d := make([]byte, 32)
	sum := strings.Repeat("00", 32)
	st := func(pt, name, sha string) *statement {
		s := &statement{Type: statementV1, PredicateType: pt}
		s.Subject = append(s.Subject, struct {
			Name   string            `json:"name"`
			Digest map[string]string `json:"digest"`
		}{name, map[string]string{"sha256": sha}})
		return s
	}
	for name, tc := range map[string]struct {
		st   *statement
		file string
		ok   bool
	}{
		"slsa":            {st: st(PredicateSLSAv1, fileA, sum), file: fileA, ok: true},
		"any name":        {st: st(PredicateSLSAv1, fileA, sum), ok: true},
		"upper case hex":  {st: st(PredicateSLSAv1, fileA, strings.ToUpper(sum)), file: fileA, ok: true},
		"sbom":            {st: st("https://spdx.dev/Document/v2.3", fileA, sum), file: fileA},
		"no statement":    {file: fileA},
		"other name":      {st: st(PredicateSLSAv1, "b.tar.gz", sum), file: fileA},
		"name, other sum": {st: st(PredicateSLSAv1, fileA, strings.Repeat("11", 32)), file: fileA},
		"other sum":       {st: st(PredicateSLSAv1, fileA, strings.Repeat("11", 32))},
	} {
		if err := checkStatement(tc.st, d, tc.file); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func goodExtensions(p Policy) fulcioExtensions {
	return fulcioExtensions{
		Issuer: Issuer, SourceRepositoryDigest: strings.Repeat("ab", 20), RunnerEnvironment: GitHubHosted,
		SourceRepositoryURI: "https://github.com/" + p.Owner + "/" + p.Repo, SourceRepositoryRef: "refs/tags/" + p.Tag,
		SourceRepositoryIdentifier: p.RepoID, SourceRepositoryOwnerURI: "https://github.com/" + p.Owner,
		SourceRepositoryOwnerIdentifier: p.OwnerID, BuildConfigURI: p.SAN(), BuildTrigger: "push",
	}
}

func TestPolicyRequiresHostedRunnerAndPush(t *testing.T) {
	t.Parallel()
	p := ReleasePolicy("v1.2.3")
	good := goodExtensions(p)
	if err := p.checkCertificate(p.SAN(), good); err != nil {
		t.Fatalf("refused the matching certificate: %v", err)
	}
	for name, f := range map[string]func(*fulcioExtensions){
		"self-hosted runner": func(e *fulcioExtensions) { e.RunnerEnvironment = "self-hosted" },
		"workflow_dispatch":  func(e *fulcioExtensions) { e.BuildTrigger = "workflow_dispatch" },
		"other issuer":       func(e *fulcioExtensions) { e.Issuer = "https://example.com" },
		"branch ref":         func(e *fulcioExtensions) { e.SourceRepositoryRef = "refs/heads/main" },
		"other build config": func(e *fulcioExtensions) { e.BuildConfigURI += "x" },
		"no commit":          func(e *fulcioExtensions) { e.SourceRepositoryDigest = "" },
	} {
		e := good
		f(&e)
		if err := p.checkCertificate(p.SAN(), e); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := p.checkCertificate(p.SAN()+"x", good); err == nil {
		t.Error("accepted another SAN")
	}
}

func TestPolicyAcceptsTheNameBeforeTheRename(t *testing.T) {
	t.Parallel()
	p := ReleasePolicy("v1.2.3")
	legacy := p
	legacy.Repo = LegacyRepo
	e := goodExtensions(legacy)
	if err := p.checkCertificate(legacy.SAN(), e); err != nil {
		t.Fatalf("refused a release built under %s: %v", LegacyRepo, err)
	}
	mixed := e
	mixed.SourceRepositoryURI = "https://github.com/" + Owner + "/" + Repo
	if err := p.checkCertificate(legacy.SAN(), mixed); err == nil {
		t.Error("accepted a certificate whose SAN and source repository name different repositories")
	}
	other := e
	other.SourceRepositoryIdentifier = "1"
	if err := p.checkCertificate(legacy.SAN(), other); err == nil {
		t.Error("accepted another repository id")
	}
	stranger := p
	stranger.Repo = "stranger"
	se := goodExtensions(stranger)
	if err := p.checkCertificate(stranger.SAN(), se); err == nil {
		t.Error("accepted a repository name that the policy does not list")
	}
}

func TestNextLink(t *testing.T) {
	t.Parallel()
	if got := nextLink(`<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=9>; rel="last"`); got != "https://api.github.com/x?page=2" {
		t.Errorf("nextLink = %q", got)
	}
	if got := nextLink(`<https://api.github.com/x?page=1>; rel="prev"`); got != "" {
		t.Errorf("nextLink = %q", got)
	}
}

func TestCheckJSON(t *testing.T) {
	t.Parallel()
	for doc, ok := range map[string]bool{
		`{"a":1,"b":{"a":2},"c":[{"a":1},{"a":2}]}`: true,
		`{"a":1,"a":2}`:             false,
		`{"subject":1,"Subject":2}`: false,
		`{"s":1,"ſ":2}`:             false,
		`{"S":1,"ſ":2}`:             false,
		`{"b":{"x":1,"x":2}}`:       false,
		`{"a":1} {"a":2}`:           false,
		`[1,2]`:                     true,
		`{"a":[{"k":1}],"k":2}`:     true,
		`{"a":`:                     false,
		strings.Repeat("[", 65) + strings.Repeat("]", 65): false,
	} {
		if err := checkJSON([]byte(doc)); (err == nil) != ok {
			t.Errorf("checkJSON(%s) = %v", doc, err)
		}
	}
}

func TestDecodeSnappy(t *testing.T) {
	t.Parallel()
	// "abcabcabcd": literal "abc", copy of 6 at offset 3, literal "d".
	data := []byte{10, 2 << 2, 'a', 'b', 'c', 1 | (6-4)<<2, 3, 0, 'd'}
	got, err := decodeSnappy(data, 100)
	if err != nil || string(got) != "abcabcabcd" {
		t.Fatalf("decodeSnappy = %q, %v", got, err)
	}
	for name, bad := range map[string][]byte{
		"too long":    {200, 1},
		"bad offset":  {4, 1, 9},
		"short":       {5, 0, 'a'},
		"truncated":   {4, 2 << 2, 'a'},
		"over length": {1, 1 << 2, 'a', 'b'},
	} {
		if _, err := decodeSnappy(bad, 100); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func TestInclusionProof(t *testing.T) {
	t.Parallel()
	for size := 1; size <= 17; size++ {
		leaves := make([][]byte, size)
		for i := range leaves {
			h := sha256.Sum256([]byte{0, byte(i)})
			leaves[i] = h[:]
		}
		root := merkleRoot(leaves)
		for i := range size {
			path := merklePath(i, leaves)
			got, err := rootFromInclusionProof(uint64(i), uint64(size), leaves[i], path)
			if err != nil || !bytes.Equal(got, root) {
				t.Fatalf("size %d index %d: %x, %v", size, i, got, err)
			}
			if len(path) > 0 {
				if got, err := rootFromInclusionProof(uint64(i), uint64(size), leaves[i], path[1:]); err == nil && bytes.Equal(got, root) {
					t.Fatalf("size %d index %d: a short proof verified", size, i)
				}
			}
		}
	}
}

// merkleRoot and merklePath are MTH and PATH of RFC 6962.
func merkleRoot(leaves [][]byte) []byte {
	if len(leaves) == 1 {
		return leaves[0]
	}
	k := split(len(leaves))
	return hashChildren(merkleRoot(leaves[:k]), merkleRoot(leaves[k:]))
}

func merklePath(m int, leaves [][]byte) [][]byte {
	if len(leaves) <= 1 {
		return nil
	}
	k := split(len(leaves))
	if m < k {
		return append(merklePath(m, leaves[:k]), merkleRoot(leaves[k:]))
	}
	return append(merklePath(m-k, leaves[k:]), merkleRoot(leaves[:k]))
}

func split(n int) int {
	k := 1
	for k*2 < n {
		k *= 2
	}
	return k
}

// sigstore is a test Sigstore instance: a Fulcio root and intermediate,
// a CT log and a Rekor log, and the trusted root that names them.
type sigstore struct {
	ctKey, rekorKey *ecdsa.PrivateKey
	interKey        *ecdsa.PrivateKey
	inter           *x509.Certificate
	trusted         *TrustedRoot
}

func newKey(t *testing.T, c elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustCert(t *testing.T, tmpl, parent *x509.Certificate, pub any, priv *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func spki(t *testing.T, k *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(k.Public())
	if err != nil {
		t.Fatal(err)
	}
	return der
}

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) //nolint:gochecknoglobals // a fixed time

// newCA makes a Fulcio-like root and code-signing intermediate.
func newCA(t *testing.T, name string) (root *x509.Certificate, interKey *ecdsa.PrivateKey, inter *x509.Certificate) {
	t.Helper()
	rootKey := newKey(t, elliptic.P384())
	interKey = newKey(t, elliptic.P384())
	root = mustCert(t, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{Organization: []string{name}, CommonName: "root"},
		NotBefore: epoch, NotAfter: epoch.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{Organization: []string{name}, CommonName: "root"},
		NotBefore: epoch, NotAfter: epoch.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}, rootKey.Public(), rootKey)
	inter = mustCert(t, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{Organization: []string{name}, CommonName: "intermediate"},
		NotBefore: epoch, NotAfter: epoch.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}, root, interKey.Public(), rootKey)
	return root, interKey, inter
}

func newSigstore(t *testing.T) *sigstore {
	t.Helper()
	s := &sigstore{ctKey: newKey(t, elliptic.P256()), rekorKey: newKey(t, elliptic.P256())}
	var root *x509.Certificate
	root, s.interKey, s.inter = newCA(t, "test")
	logEntry := func(k *ecdsa.PrivateKey) map[string]any {
		der := spki(t, k)
		id := sha256.Sum256(der)
		return map[string]any{
			"publicKey": map[string]any{"rawBytes": der, "validFor": map[string]any{"start": epoch}},
			"logId":     map[string]any{"keyId": id[:]},
		}
	}
	doc, err := json.Marshal(map[string]any{
		"mediaType": "application/vnd.dev.sigstore.trustedroot+json;version=0.1",
		"tlogs":     []any{logEntry(s.rekorKey)},
		"ctlogs":    []any{logEntry(s.ctKey)},
		"certificateAuthorities": []any{map[string]any{
			"certChain": map[string]any{"certificates": []any{map[string]any{"rawBytes": s.inter.Raw}, map[string]any{"rawBytes": root.Raw}}},
			"validFor":  map[string]any{"start": epoch},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.trusted, err = ParseTrustedRoot(doc); err != nil {
		t.Fatal(err)
	}
	return s
}

func testPolicy() Policy {
	return Policy{Owner: "alice", Repo: "widget", OwnerID: "11", RepoID: "22", Workflow: ".github/workflows/release.yaml", Tag: "v1.2.3", Name: fileA}
}

var testDigest = sha256.Sum256([]byte("archive")) //nolint:gochecknoglobals // a fixed digest

// spec says how to sign a bundle. Its zero value is a bundle that passes
// testPolicy for testDigest; each field breaks or varies one thing.
type spec struct {
	ext        map[int]string // DER extensions to set; "" removes one
	rawExt     map[int]string // raw extensions 1, 2, 6 to set; "" removes one
	extTag     int
	sans       []string
	integrated time.Time
	validFrom  time.Time
	statement  map[string]any
	payload    []byte
	signOver   []byte
	payloadTyp string
	kind       string
	version    int
	noSCT      bool
	sctKey     *ecdsa.PrivateKey
	sctBadSig  bool
	sctTime    time.Time
	otherCA    bool
	badSET     bool
	setKey     *ecdsa.PrivateKey
	unknownLog bool
	noProof    bool
	badProof   bool
	badRoot    bool
	ckptKey    *ecdsa.PrivateKey
	bodyHash   string
	bodySig    []byte
	bodyCert   bool
	bodyKind   string
	edit       func(b map[string]any)
}

func (sp *spec) defaults() {
	if sp.validFrom.IsZero() {
		sp.validFrom = epoch.Add(24 * time.Hour)
	}
	if sp.integrated.IsZero() {
		sp.integrated = sp.validFrom.Add(2 * time.Minute)
	}
	if sp.sctTime.IsZero() {
		sp.sctTime = sp.validFrom
	}
	if sp.kind == "" {
		sp.kind = "dsse"
	}
	if sp.version == 0 {
		sp.version = 3
	}
	if sp.payloadTyp == "" {
		sp.payloadTyp = payloadTypeInToto
	}
	if sp.sans == nil {
		sp.sans = []string{testPolicy().SAN()}
	}
}

func (sp *spec) extensions(t *testing.T) []pkix.Extension {
	t.Helper()
	ext := map[int]string{
		8: Issuer, 11: GitHubHosted, 12: "https://github.com/alice/widget", 13: strings.Repeat("ab", 20),
		14: "refs/tags/v1.2.3", 15: "22", 16: "https://github.com/alice", 17: "11", 18: testPolicy().SAN(), 20: "push",
	}
	rawExt := map[int]string{1: Issuer, 2: "push", 6: "refs/tags/v1.2.3"}
	for m, set := range map[*map[int]string]map[int]string{&ext: sp.ext, &rawExt: sp.rawExt} {
		for k, v := range set {
			if v == "" {
				delete(*m, k)
			} else {
				(*m)[k] = v
			}
		}
	}
	var exts []pkix.Extension
	for arc := 1; arc <= 24; arc++ {
		id := append(append(asn1.ObjectIdentifier{}, oidFulcio...), arc)
		if v, ok := rawExt[arc]; ok {
			exts = append(exts, pkix.Extension{Id: id, Value: []byte(v)})
		}
		if v, ok := ext[arc]; ok {
			params := "utf8"
			if sp.extTag == asn1.TagIA5String {
				params = "ia5"
			}
			der, err := asn1.MarshalWithParams(v, params)
			if err != nil {
				t.Fatal(err)
			}
			exts = append(exts, pkix.Extension{Id: id, Value: der})
		}
	}
	return exts
}

// leaf issues the signing certificate, with an embedded SCT.
func (s *sigstore) leaf(t *testing.T, sp *spec, key *ecdsa.PrivateKey) (*x509.Certificate, *x509.Certificate, *x509.Certificate) {
	t.Helper()
	uris := make([]*url.URL, 0, len(sp.sans))
	for _, v := range sp.sans {
		u, err := url.Parse(v)
		if err != nil {
			t.Fatal(err)
		}
		uris = append(uris, u)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3), NotBefore: sp.validFrom, NotAfter: sp.validFrom.Add(10 * time.Minute),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs: uris, ExtraExtensions: sp.extensions(t),
	}
	interKey, inter := s.interKey, s.inter
	if sp.otherCA {
		_, interKey, inter = newCA(t, "other")
	}
	pre := mustCert(t, tmpl, inter, key.Public(), interKey)
	if sp.noSCT {
		return pre, mustCert(t, tmpl, inter, key.Public(), interKey), inter
	}
	ctKey := s.ctKey
	if sp.sctKey != nil {
		ctKey = sp.sctKey
	}
	logID := sha256.Sum256(spki(t, ctKey))
	ts := uint64(sp.sctTime.UnixMilli())
	ikh := sha256.Sum256(inter.RawSubjectPublicKeyInfo)
	tbs := pre.RawTBSCertificate
	msg := []byte{0, 0}
	msg = binary.BigEndian.AppendUint64(msg, ts)
	msg = append(msg, 0, 1)
	msg = append(msg, ikh[:]...)
	msg = append(msg, byte(len(tbs)>>16&0xff), byte(len(tbs)>>8&0xff), byte(len(tbs)&0xff))
	msg = append(msg, tbs...)
	msg = append(msg, 0, 0)
	if sp.sctBadSig {
		msg = append(msg, 0)
	}
	sum := sha256.Sum256(msg)
	sig, err := ecdsa.SignASN1(rand.Reader, ctKey, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	one := append([]byte{0}, logID[:]...)
	one = binary.BigEndian.AppendUint64(one, ts)
	one = append(one, 0, 0, 4, 3)
	one = binary.BigEndian.AppendUint16(one, uint16(min(len(sig), math.MaxUint16)))
	one = append(one, sig...)
	list := binary.BigEndian.AppendUint16(nil, uint16(min(len(one), math.MaxUint16)))
	list = append(list, one...)
	list = append(binary.BigEndian.AppendUint16(nil, uint16(min(len(list), math.MaxUint16))), list...)
	val, err := asn1.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	tmpl.ExtraExtensions = append(tmpl.ExtraExtensions, pkix.Extension{Id: oidSCTList, Value: val})
	final := mustCert(t, tmpl, inter, key.Public(), interKey)
	if got, err := precertTBS(final.RawTBSCertificate); err != nil || !bytes.Equal(got, tbs) {
		t.Fatalf("precertTBS does not undo the SCT extension: %v", err)
	}
	return final, mustCert(t, tmpl, inter, key.Public(), interKey), inter
}

func (sp *spec) statementPayload(t *testing.T) []byte {
	t.Helper()
	if sp.payload != nil {
		return sp.payload
	}
	st := map[string]any{
		"_type":         statementV1,
		"predicateType": PredicateSLSAv1,
		"subject": []any{
			map[string]any{"name": "other.tar.gz", "digest": map[string]any{"sha256": strings.Repeat("01", 32)}},
			map[string]any{"name": fileA, "digest": map[string]any{"sha256": hex.EncodeToString(testDigest[:])}},
		},
		"predicate": map[string]any{"buildDefinition": map[string]any{}},
	}
	for k, v := range sp.statement {
		st[k] = v
	}
	payload, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func (sp *spec) body(t *testing.T, payload, sig []byte, cert *x509.Certificate) []byte {
	t.Helper()
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	ph := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(ph[:])
	if sp.bodyHash != "" {
		payloadHash = sp.bodyHash
	}
	if sp.bodySig != nil {
		sig = sp.bodySig
	}
	var body map[string]any
	switch sp.kind {
	case "dsse":
		body = map[string]any{"apiVersion": "0.0.1", "kind": "dsse", "spec": map[string]any{
			"envelopeHash": map[string]any{"algorithm": "sha256", "value": strings.Repeat("00", 32)},
			"payloadHash":  map[string]any{"algorithm": "sha256", "value": payloadHash},
			"signatures":   []any{map[string]any{"signature": base64.StdEncoding.EncodeToString(sig), "verifier": certPEM}},
		}}
	case "intoto":
		body = map[string]any{"apiVersion": "0.0.2", "kind": "intoto", "spec": map[string]any{"content": map[string]any{
			"envelope": map[string]any{
				"payloadType": sp.payloadTyp,
				"signatures":  []any{map[string]any{"sig": []byte(base64.StdEncoding.EncodeToString(sig)), "publicKey": certPEM}},
			},
			"hash":        map[string]any{"algorithm": "sha256", "value": strings.Repeat("00", 32)},
			"payloadHash": map[string]any{"algorithm": "sha256", "value": payloadHash},
		}}}
	}
	canon, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return canon
}

// entry logs body in a 7-entry log at shard index 4, with a signed entry
// timestamp, an inclusion proof and a checkpoint.
func (s *sigstore) entry(t *testing.T, sp *spec, canon []byte) map[string]any {
	t.Helper()
	rekorKey := s.rekorKey
	if sp.unknownLog {
		rekorKey = newKey(t, elliptic.P256())
	}
	logID := sha256.Sum256(spki(t, rekorKey))
	const shardIndex, size, logIndex = 4, 7, 1004
	setPayload := fmt.Sprintf(`{"body":%q,"integratedTime":%d,"logID":%q,"logIndex":%d}`,
		base64.StdEncoding.EncodeToString(canon), sp.integrated.Unix(), hex.EncodeToString(logID[:]), logIndex)
	setKey := rekorKey
	if sp.setKey != nil {
		setKey = sp.setKey
	}
	setSum := sha256.Sum256([]byte(setPayload))
	set, err := ecdsa.SignASN1(rand.Reader, setKey, setSum[:])
	if err != nil {
		t.Fatal(err)
	}
	if sp.badSET {
		set[len(set)-1] ^= 1
	}
	kind, version := sp.kind, map[string]string{"dsse": "0.0.1", "intoto": "0.0.2"}[sp.kind]
	if sp.bodyKind != "" {
		kind = sp.bodyKind
	}
	e := map[string]any{
		"logIndex":          fmt.Sprint(logIndex),
		"logId":             map[string]any{"keyId": logID[:]},
		"kindVersion":       map[string]any{"kind": kind, "version": version},
		"integratedTime":    fmt.Sprint(sp.integrated.Unix()),
		"inclusionPromise":  map[string]any{"signedEntryTimestamp": set},
		"canonicalizedBody": canon,
	}
	if sp.noProof {
		return e
	}
	leaves := make([][]byte, size)
	for i := range leaves {
		h := sha256.Sum256([]byte{0, byte(i)})
		leaves[i] = h[:]
	}
	lh := sha256.Sum256(append([]byte{0}, canon...))
	leaves[shardIndex] = lh[:]
	root := merkleRoot(leaves)
	path := merklePath(shardIndex, leaves)
	if sp.badProof {
		path[0] = bytes.Repeat([]byte{1}, 32)
	}
	ckptRoot := root
	if sp.badRoot {
		ckptRoot = bytes.Repeat([]byte{2}, 32)
	}
	text := fmt.Sprintf("rekor.example - 1\n%d\n%s\n", size, base64.StdEncoding.EncodeToString(ckptRoot))
	ckptKey := rekorKey
	if sp.ckptKey != nil {
		ckptKey = sp.ckptKey
	}
	ts := sha256.Sum256([]byte(text))
	csig, err := ecdsa.SignASN1(rand.Reader, ckptKey, ts[:])
	if err != nil {
		t.Fatal(err)
	}
	kh := sha256.Sum256(spki(t, ckptKey))
	e["inclusionProof"] = map[string]any{
		"logIndex": fmt.Sprint(shardIndex), "rootHash": root, "treeSize": fmt.Sprint(size), "hashes": path,
		"checkpoint": map[string]any{"envelope": text + "\n— rekor.example " + base64.StdEncoding.EncodeToString(append(kh[:4:4], csig...)) + "\n"},
	}
	return e
}

func (s *sigstore) sign(t *testing.T, sp spec) []byte {
	t.Helper()
	sp.defaults()
	leafKey := newKey(t, elliptic.P256())
	leaf, twin, inter := s.leaf(t, &sp, leafKey)
	payload := sp.statementPayload(t)
	signed := pae(sp.payloadTyp, payload)
	if sp.signOver != nil {
		signed = sp.signOver
	}
	sum := sha256.Sum256(signed)
	sig, err := ecdsa.SignASN1(rand.Reader, leafKey, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	bodyCert := leaf
	if sp.bodyCert {
		bodyCert = twin
	}
	vm := map[string]any{"tlogEntries": []any{s.entry(t, &sp, sp.body(t, payload, sig, bodyCert))}}
	mediaType := "application/vnd.dev.sigstore.bundle.v0.3+json"
	if sp.version == 3 {
		vm["certificate"] = map[string]any{"rawBytes": leaf.Raw}
	} else {
		mediaType = fmt.Sprintf("application/vnd.dev.sigstore.bundle+json;version=0.%d", sp.version)
		vm["x509CertificateChain"] = map[string]any{"certificates": []any{map[string]any{"rawBytes": leaf.Raw}, map[string]any{"rawBytes": inter.Raw}}}
	}
	b := map[string]any{
		"mediaType":            mediaType,
		"verificationMaterial": vm,
		"dsseEnvelope": map[string]any{
			"payload": payload, "payloadType": sp.payloadTyp,
			"signatures": []any{map[string]any{"sig": sig}},
		},
	}
	if sp.edit != nil {
		sp.edit(b)
	}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func tlogEntryOf(b map[string]any) map[string]any {
	return b["verificationMaterial"].(map[string]any)["tlogEntries"].([]any)[0].(map[string]any)
}

func TestVerifySignedBundles(t *testing.T) {
	t.Parallel()
	s := newSigstore(t)
	other := newKey(t, elliptic.P256())
	now := epoch.AddDate(0, 1, 0)
	digestOther := []any{map[string]any{"name": fileA, "digest": map[string]any{"sha256": strings.Repeat("02", 32)}}}
	nameOther := []any{map[string]any{"name": "b.tar.gz", "digest": map[string]any{"sha256": hex.EncodeToString(testDigest[:])}}}
	for name, tc := range map[string]struct {
		spec spec
		want string
		is   error
	}{
		"dsse entry":           {},
		"intoto entry":         {spec: spec{kind: "intoto"}},
		"bundle v0.2 chain":    {spec: spec{version: 2}},
		"bundle v0.1 no proof": {spec: spec{version: 1, noProof: true}},
		"no checkpoint": {spec: spec{edit: func(b map[string]any) {
			delete(tlogEntryOf(b)["inclusionProof"].(map[string]any), "checkpoint")
		}}, want: "no checkpoint"},
		"v0.3 with a chain": {spec: spec{edit: func(b map[string]any) {
			vm := b["verificationMaterial"].(map[string]any)
			vm["x509CertificateChain"] = map[string]any{"certificates": []any{vm["certificate"]}}
			delete(vm, "certificate")
		}}, want: "not a chain"},

		"bad DSSE signature": {spec: spec{signOver: []byte("something else")}, want: "DSSE signature does not verify"},
		"DSSE signed as raw": {spec: spec{signOver: []byte(`{"_type":"x"}`)}, want: "DSSE signature does not verify"},
		"other payload type": {spec: spec{payloadTyp: "application/json"}, want: "DSSE payload type"},
		"two DSSE signatures": {spec: spec{edit: func(b map[string]any) {
			e := b["dsseEnvelope"].(map[string]any)
			e["signatures"] = append(e["signatures"].([]any), e["signatures"].([]any)[0])
		}}, want: "2 signatures"},
		"wrong chain":            {spec: spec{otherCA: true}, want: "does not chain", is: ErrUntrustedRoot},
		"missing SCT":            {spec: spec{noSCT: true}, want: "no signed certificate timestamp"},
		"SCT of an unknown log":  {spec: spec{sctKey: other}, want: "not a trusted CT log"},
		"bad SCT signature":      {spec: spec{sctBadSig: true}, want: "SCT 1: the signature does not verify"},
		"SCT before the log key": {spec: spec{sctTime: epoch.Add(-time.Hour)}, want: "CT log key was not valid"},
		"bad SET":                {spec: spec{badSET: true}, want: "signed entry timestamp does not verify"},
		"SET of another key":     {spec: spec{setKey: other}, want: "signed entry timestamp does not verify"},
		"no SET":                 {spec: spec{edit: func(b map[string]any) { delete(tlogEntryOf(b), "inclusionPromise") }}, want: "no signed entry timestamp"},
		"SET over another time": {spec: spec{edit: func(b map[string]any) {
			tlogEntryOf(b)["integratedTime"] = fmt.Sprint(epoch.Add(24*time.Hour + 3*time.Minute).Unix())
		}}, want: "signed entry timestamp does not verify"},
		"unknown log":              {spec: spec{unknownLog: true}, want: "not in the trusted root", is: ErrUntrustedRoot},
		"no tlog entry":            {spec: spec{edit: func(b map[string]any) { b["verificationMaterial"].(map[string]any)["tlogEntries"] = []any{} }}, want: "no transparency log entry"},
		"body payload hash":        {spec: spec{bodyHash: strings.Repeat("00", 32)}, want: "payload hash does not match"},
		"body signature":           {spec: spec{bodySig: []byte("x")}, want: "signature does not match"},
		"body certificate":         {spec: spec{bodyCert: true}, want: "certificate does not match"},
		"intoto body certificate":  {spec: spec{kind: "intoto", bodyCert: true}, want: "certificate does not match"},
		"intoto body hash":         {spec: spec{kind: "intoto", bodyHash: strings.Repeat("00", 32)}, want: "payload hash does not match"},
		"intoto body signature":    {spec: spec{kind: "intoto", bodySig: []byte("x")}, want: "signature does not match"},
		"body kind":                {spec: spec{bodyKind: "intoto"}, want: "the bundle says intoto"},
		"expired at integration":   {spec: spec{integrated: epoch.Add(24*time.Hour + 11*time.Minute)}, want: "not valid at the log's integrated time"},
		"before validity":          {spec: spec{integrated: epoch.Add(24*time.Hour - time.Minute)}, want: "not valid at the log's integrated time"},
		"integrated in the future": {spec: spec{validFrom: now.Add(time.Hour)}, want: "in the future"},
		"bad inclusion proof":      {spec: spec{badProof: true}, want: "inclusion proof does not lead"},
		"checkpoint root":          {spec: spec{badRoot: true}, want: "checkpoint root hash"},
		"checkpoint key":           {spec: spec{ckptKey: other}, want: "no checkpoint signature verifies"},
		"v0.3 without proof":       {spec: spec{noProof: true}, want: "no inclusion proof"},
		"public key bundle": {spec: spec{edit: func(b map[string]any) {
			b["verificationMaterial"].(map[string]any)["publicKey"] = map[string]any{"hint": "x"}
		}}, want: "public key"},
		"message signature":    {spec: spec{edit: func(b map[string]any) { b["messageSignature"] = map[string]any{} }}, want: "no message signature"},
		"unknown media type":   {spec: spec{edit: func(b map[string]any) { b["mediaType"] = "application/vnd.dev.sigstore.bundle.v0.4+json" }}, want: "not supported"},
		"duplicate bundle key": {spec: spec{edit: func(b map[string]any) { b["MediaType"] = b["mediaType"] }}, want: "duplicate JSON key"},

		"wrong SAN":               {spec: spec{sans: []string{"https://github.com/alice/widget/.github/workflows/release.yaml@refs/heads/main"}}, want: "certificate SAN"},
		"two SANs":                {spec: spec{sans: []string{testPolicy().SAN(), "https://example.com/"}}, want: "2 subject alternative names"},
		"wrong issuer":            {spec: spec{ext: map[int]string{8: "https://example.com"}, rawExt: map[int]string{1: "https://example.com"}}, want: "OIDC issuer"},
		"raw issuer only":         {spec: spec{ext: map[int]string{8: ""}}, want: "disagree"},
		"raw issuer disagrees":    {spec: spec{rawExt: map[int]string{1: "https://example.com"}}, want: "disagree"},
		"raw ref disagrees":       {spec: spec{rawExt: map[int]string{6: "refs/heads/main"}}, want: "disagree"},
		"no raw extensions":       {spec: spec{rawExt: map[int]string{1: "", 2: "", 6: ""}}},
		"wrong build config":      {spec: spec{ext: map[int]string{18: "https://github.com/alice/widget/.github/workflows/other.yaml@refs/tags/v1.2.3"}}, want: "build config URI"},
		"wrong repo URI":          {spec: spec{ext: map[int]string{12: "https://github.com/alice/other"}}, want: "source repository URI"},
		"wrong repo ID":           {spec: spec{ext: map[int]string{15: "23"}}, want: "source repository identifier"},
		"wrong owner URI":         {spec: spec{ext: map[int]string{16: "https://github.com/bob"}}, want: "source repository owner URI"},
		"wrong owner ID":          {spec: spec{ext: map[int]string{17: "12"}}, want: "source repository owner identifier"},
		"wrong ref":               {spec: spec{ext: map[int]string{14: "refs/tags/v1.2.4"}, rawExt: map[int]string{6: "refs/tags/v1.2.4"}}, want: "source repository ref"},
		"wrong trigger":           {spec: spec{ext: map[int]string{20: "workflow_dispatch"}, rawExt: map[int]string{2: "workflow_dispatch"}}, want: "build trigger"},
		"self-hosted runner":      {spec: spec{ext: map[int]string{11: "self-hosted"}}, want: "runner environment"},
		"no commit":               {spec: spec{ext: map[int]string{13: ""}}, want: "source repository digest"},
		"missing ref":             {spec: spec{ext: map[int]string{14: ""}, rawExt: map[int]string{6: ""}}, want: "source repository ref"},
		"IA5String extension":     {spec: spec{extTag: asn1.TagIA5String}, want: "not a DER UTF8String"},
		"wrong predicate":         {spec: spec{statement: map[string]any{"predicateType": "https://slsa.dev/provenance/v0.2"}}, want: "predicate type"},
		"statement v0.1":          {spec: spec{statement: map[string]any{"_type": "https://in-toto.io/Statement/v0.1"}}, want: "statement type"},
		"digest not in subjects":  {spec: spec{statement: map[string]any{"subject": digestOther}}, want: "artifact digest"},
		"digest under other name": {spec: spec{statement: map[string]any{"subject": nameOther}}, want: `no subject named "a.tar.gz"`},
		"duplicate statement key": {spec: spec{payload: []byte(`{"_type":"` + statementV1 + `","predicateType":"x","predicateType":"` + PredicateSLSAv1 + `","subject":[]}`)}, want: "duplicate JSON key"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res, err := verifyOne(s.trusted, s.sign(t, tc.spec), now, testDigest[:], testPolicy())
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if res.SAN != testPolicy().SAN() || res.Commit != strings.Repeat("ab", 20) {
					t.Errorf("result %+v", res)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("verifyOne = %+v, %v; want an error with %q", res, err, tc.want)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("error %v is not %v", err, tc.is)
			}
		})
	}
}

func TestVerifyOtherTagRefused(t *testing.T) {
	t.Parallel()
	s := newSigstore(t)
	p := testPolicy()
	p.Tag = "v1.2.4"
	if _, err := verifyOne(s.trusted, s.sign(t, spec{}), epoch.AddDate(0, 1, 0), testDigest[:], p); err == nil || !strings.Contains(err.Error(), "certificate SAN") {
		t.Fatalf("verifyOne = %v", err)
	}
}

func TestPublicGoodRoot(t *testing.T) {
	t.Parallel()
	tr := PublicGoodRoot()
	if len(tr.cas) != 2 || len(tr.tlogs) != 2 || len(tr.ctlogs) != 2 {
		t.Fatalf("trusted root has %d CAs, %d logs, %d CT logs", len(tr.cas), len(tr.tlogs), len(tr.ctlogs))
	}
	sum := sha256.Sum256(publicGoodRootJSON)
	if got := hex.EncodeToString(sum[:]); got != "6494e21ea73fa7ee769f85f57d5a3e6a08725eae1e38c755fc3517c9e6bc0b66" {
		t.Errorf("trusted_root.json has sha256 %s; record its source and date in trustedroot.go when it changes", got)
	}
}
