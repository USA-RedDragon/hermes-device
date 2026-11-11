// Package attest verifies the GitHub artifact attestations of hermes-device
// release files: SLSA build provenance that the release workflow signs with
// the Sigstore public-good instance, as `gh attestation verify` checks it.
package attest

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	in_toto "github.com/in-toto/attestation/go/v1"
	"github.com/klauspost/compress/snappy"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// The release workflow of this repository, and the numeric IDs of the
// repository and its owner, which survive a rename.
const (
	Owner    = "USA-RedDragon"
	Repo     = "hermes-device"
	OwnerID  = "13051767"
	RepoID   = "1452996986"
	Workflow = ".github/workflows/release.yaml"
)

// Constants of the Sigstore certificates that GitHub Actions gets.
const (
	Issuer          = "https://token.actions.githubusercontent.com"
	GitHubHosted    = "github-hosted"
	PredicateSLSAv1 = "https://slsa.dev/provenance/v1"
	DefaultAPIURL   = "https://api.github.com"
)

// Limits on what the API and the bundle store send.
const (
	maxAPIBody      = 8 << 20
	maxBundle       = 4 << 20
	pageSize        = 30
	maxPages        = 5
	maxAttestations = pageSize * maxPages
)

// Verification errors.
var (
	ErrNoAttestation = errors.New("GitHub has no attestation for this file")
	ErrNotVerified   = errors.New("no attestation of this file passed verification")
	ErrPolicy        = errors.New("invalid attestation policy")
)

var tagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// Policy is who must have signed an attestation: the workflow file of a
// repository, run on GitHub-hosted runners for a push of tag Tag.
type Policy struct {
	Owner, Repo     string
	OwnerID, RepoID string
	Workflow        string
	Tag             string
	// Name, when set, must be the name of the statement subject that has
	// the digest, so that another file of the same release does not pass.
	Name string
}

// ReleasePolicy is the policy for release tag of this repository.
func ReleasePolicy(tag string) Policy {
	return Policy{Owner: Owner, Repo: Repo, OwnerID: OwnerID, RepoID: RepoID, Workflow: Workflow, Tag: tag}
}

// SAN is the certificate subject the policy requires: the workflow file at
// the tag ref.
func (p Policy) SAN() string {
	return "https://github.com/" + p.Owner + "/" + p.Repo + "/" + p.Workflow + "@refs/tags/" + p.Tag
}

func (p Policy) identity() (verify.CertificateIdentity, error) {
	if !tagPattern.MatchString(p.Tag) {
		return verify.CertificateIdentity{}, fmt.Errorf("%w: %q is not a release tag", ErrPolicy, p.Tag)
	}
	if p.Owner == "" || p.Repo == "" || p.OwnerID == "" || p.RepoID == "" || !strings.HasPrefix(p.Workflow, ".github/workflows/") {
		return verify.CertificateIdentity{}, fmt.Errorf("%w: %+v", ErrPolicy, p)
	}
	san, err := verify.NewSANMatcher(p.SAN(), "")
	if err != nil {
		return verify.CertificateIdentity{}, err
	}
	issuer, err := verify.NewIssuerMatcher(Issuer, "")
	if err != nil {
		return verify.CertificateIdentity{}, err
	}
	repoURL := "https://github.com/" + p.Owner + "/" + p.Repo
	return verify.NewCertificateIdentity(san, issuer, certificate.Extensions{
		BuildConfigURI:                  p.SAN(),
		RunnerEnvironment:               GitHubHosted,
		SourceRepositoryURI:             repoURL,
		SourceRepositoryIdentifier:      p.RepoID,
		SourceRepositoryOwnerURI:        "https://github.com/" + p.Owner,
		SourceRepositoryOwnerIdentifier: p.OwnerID,
		SourceRepositoryRef:             "refs/tags/" + p.Tag,
		BuildTrigger:                    "push",
	})
}

// Verifier fetches attestations from the GitHub API and verifies them.
type Verifier struct {
	// APIURL is DefaultAPIURL, or a test server.
	APIURL    string
	Client    *http.Client
	UserAgent string
	// TrustedRoot returns the Sigstore keys and certificates to trust;
	// nil means the public-good instance's, from its TUF repository.
	TrustedRoot func(ctx context.Context) (root.TrustedMaterial, error)
	// TUFCache is where the public-good TUF metadata is cached; empty
	// means no cache.
	TUFCache string
}

// DefaultTUFCache is the TUF cache under the user's cache directory, or ""
// when there is none.
func DefaultTUFCache() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "hermes-device", "sigstore-tuf")
}

// PublicGoodRoot returns the trusted root of the Sigstore public-good
// instance from its TUF repository, checked against the TUF root that
// sigstore-go embeds.
func PublicGoodRoot(ctx context.Context, cache string) (root.TrustedMaterial, error) {
	opts := tuf.DefaultOptions().WithContext(ctx).WithCacheValidity(1)
	if cache == "" {
		opts = opts.WithDisableLocalCache()
	} else {
		opts = opts.WithCachePath(cache)
	}
	c, err := tuf.New(opts)
	if err != nil {
		return nil, fmt.Errorf("read the Sigstore TUF repository: %w", err)
	}
	tr, err := root.GetTrustedRoot(c)
	if err != nil {
		return nil, fmt.Errorf("read the Sigstore trusted root: %w", err)
	}
	return tr, nil
}

// Result is a verified attestation.
type Result struct {
	// SAN is the certificate subject: the workflow that signed it.
	SAN string
	// Commit is the commit the workflow ran on.
	Commit string
}

// Verify checks that an attestation signed under policy p covers the file
// with SHA-256 digest. It fetches the attestations of the digest from the
// GitHub API and accepts the first one that is signed by a Sigstore
// certificate of the policy's workflow, logged in the transparency log,
// has a statement with a subject of this digest, and has SLSA provenance
// v1 as its predicate.
func (v *Verifier) Verify(ctx context.Context, digest []byte, p Policy) (*Result, error) {
	if len(digest) != 32 {
		return nil, fmt.Errorf("%w: a SHA-256 digest has 32 bytes, not %d", ErrPolicy, len(digest))
	}
	id, err := p.identity()
	if err != nil {
		return nil, err
	}
	bundles, err := v.fetch(ctx, digest, p)
	if err != nil {
		return nil, err
	}
	trustedRoot := v.TrustedRoot
	if trustedRoot == nil {
		trustedRoot = func(ctx context.Context) (root.TrustedMaterial, error) { return PublicGoodRoot(ctx, v.TUFCache) }
	}
	tm, err := trustedRoot(ctx)
	if err != nil {
		return nil, err
	}
	sv, err := verify.NewVerifier(tm, verify.WithSignedCertificateTimestamps(1), verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		return nil, err
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", digest), verify.WithCertificateIdentity(id))
	var errs []error
	for i, b := range bundles {
		res, err := sv.Verify(b, policy)
		if err != nil {
			errs = append(errs, fmt.Errorf("attestation %d: %w", i+1, err))
			continue
		}
		if err := checkStatement(res.Statement, digest, p.Name); err != nil {
			errs = append(errs, fmt.Errorf("attestation %d: %w", i+1, err))
			continue
		}
		return &Result{SAN: res.Signature.Certificate.SubjectAlternativeName, Commit: res.Signature.Certificate.SourceRepositoryDigest}, nil
	}
	return nil, fmt.Errorf("%w (sha256:%s, required signer %s): %w", ErrNotVerified, hex.EncodeToString(digest), p.SAN(), errors.Join(errs...))
}

// checkStatement checks the predicate type of a verified statement and,
// when name is set, that a subject of that name has the digest.
func checkStatement(st *in_toto.Statement, digest []byte, name string) error {
	if pt := st.GetPredicateType(); pt != PredicateSLSAv1 {
		return fmt.Errorf("predicate type %q, want %q", pt, PredicateSLSAv1)
	}
	if name == "" {
		return nil
	}
	want := hex.EncodeToString(digest)
	for _, sub := range st.GetSubject() {
		if sub.GetName() == name && strings.EqualFold(sub.GetDigest()["sha256"], want) {
			return nil
		}
	}
	return fmt.Errorf("no subject named %q has sha256:%s", name, want)
}

type apiAttestation struct {
	Bundle    json.RawMessage `json:"bundle"`
	BundleURL string          `json:"bundle_url"`
}

// get downloads u, up to limit bytes, and returns the body, the status
// and the rel="next" link.
func (v *Verifier) get(ctx context.Context, u string, api bool, limit int64) ([]byte, int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, "", err
	}
	if v.UserAgent != "" {
		req.Header.Set("User-Agent", v.UserAgent)
	}
	if api {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	c := *v.Client
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 || r.URL.Scheme != via[0].URL.Scheme {
			return errors.New("refusing the redirect")
		}
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, "", err
	}
	if int64(len(data)) > limit {
		return nil, resp.StatusCode, "", fmt.Errorf("%s: larger than %d bytes", req.URL.Redacted(), limit)
	}
	return data, resp.StatusCode, nextLink(resp.Header.Get("Link")), nil
}

// nextLink is the rel="next" URL of a Link header, or "".
func nextLink(h string) string {
	for part := range strings.SplitSeq(h, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		if ok && strings.Contains(params, `rel="next"`) {
			return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(target), "<"), ">")
		}
	}
	return ""
}

func (v *Verifier) fetch(ctx context.Context, digest []byte, p Policy) ([]*bundle.Bundle, error) {
	base := v.APIURL
	if base == "" {
		base = DefaultAPIURL
	}
	u := fmt.Sprintf("%s/repos/%s/%s/attestations/sha256:%s?per_page=%d&predicate_type=%s",
		strings.TrimSuffix(base, "/"), url.PathEscape(p.Owner), url.PathEscape(p.Repo), hex.EncodeToString(digest), pageSize, url.QueryEscape(PredicateSLSAv1))
	first, err := url.Parse(u)
	if err != nil {
		return nil, err
	}
	var all []apiAttestation
	for page := 0; u != "" && page < maxPages; page++ {
		data, status, next, err := v.get(ctx, u, true, maxAPIBody)
		if err != nil {
			return nil, fmt.Errorf("fetch attestations: %w", err)
		}
		switch {
		case status == http.StatusNotFound && page == 0:
			return nil, fmt.Errorf("%w (sha256:%s in %s/%s)", ErrNoAttestation, hex.EncodeToString(digest), p.Owner, p.Repo)
		case status != http.StatusOK:
			return nil, fmt.Errorf("fetch attestations: %s answered %d: %q", u, status, bytes.TrimSpace(data[:min(len(data), 200)]))
		}
		var body struct {
			Attestations []apiAttestation `json:"attestations"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			return nil, fmt.Errorf("fetch attestations: %w", err)
		}
		all = append(all, body.Attestations...)
		u = ""
		if nu, err := url.Parse(next); next != "" && err == nil && nu.Scheme == first.Scheme && nu.Host == first.Host {
			u = next
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("%w (sha256:%s in %s/%s)", ErrNoAttestation, hex.EncodeToString(digest), p.Owner, p.Repo)
	}
	body := struct{ Attestations []apiAttestation }{all}
	out := make([]*bundle.Bundle, 0, len(body.Attestations))
	var errs []error
	for i, a := range body.Attestations[:min(len(body.Attestations), maxAttestations)] {
		b, err := v.bundle(ctx, a)
		if err != nil {
			errs = append(errs, fmt.Errorf("attestation %d: %w", i+1, err))
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %w", ErrNotVerified, errors.Join(errs...))
	}
	return out, nil
}

// bundle reads the Sigstore bundle of an attestation: inline, or
// snappy-compressed JSON at bundle_url.
func (v *Verifier) bundle(ctx context.Context, a apiAttestation) (*bundle.Bundle, error) {
	raw := []byte(a.Bundle)
	if len(raw) == 0 || string(raw) == "null" {
		bu, err := url.Parse(a.BundleURL)
		if err != nil || bu.Scheme != "https" || bu.Host == "" {
			return nil, fmt.Errorf("no bundle and no https bundle URL")
		}
		data, status, _, err := v.get(ctx, a.BundleURL, false, maxBundle)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("bundle URL answered %d", status)
		}
		if n, err := snappy.DecodedLen(data); err != nil || n > maxBundle {
			return nil, fmt.Errorf("bundle is not snappy data of at most %d bytes", maxBundle)
		}
		if raw, err = snappy.Decode(nil, data); err != nil {
			return nil, err
		}
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return &b, nil
}
