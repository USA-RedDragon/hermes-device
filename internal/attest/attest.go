// Package attest verifies the GitHub artifact attestations of talaria
// release files: SLSA build provenance that the release workflow signs with
// the Sigstore public-good instance, as `gh attestation verify` checks it.
// It uses only the standard library and the public-good trusted root that
// is built in.
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
	"regexp"
	"strings"
	"time"
)

// The release workflow of this repository, and the numeric IDs of the
// repository and its owner, which survive a rename. LegacyRepo is the
// repository's name before it was renamed to Repo; releases built under
// either name verify.
const (
	Owner      = "USA-RedDragon"
	Repo       = "talaria"
	LegacyRepo = "hermes-device"
	OwnerID    = "13051767"
	RepoID     = "1452996986"
	Workflow   = ".github/workflows/release.yaml"
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
	// ErrUntrustedRoot is part of the error when a bundle is signed by a
	// certificate authority or logged in a log that the trusted root does
	// not have.
	ErrUntrustedRoot = errors.New("signed with Sigstore keys or certificates that this binary does not trust")
)

var (
	tagPattern    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Policy is who must have signed an attestation: the workflow file of a
// repository, run on GitHub-hosted runners for a push of tag Tag. The
// repository is the one with RepoID; its name in the certificate may be
// Repo or one of OtherNames, names it had before a rename.
type Policy struct {
	Owner, Repo     string
	OtherNames      []string
	OwnerID, RepoID string
	Workflow        string
	Tag             string
	// Name, when set, must be the name of the statement subject that has
	// the digest, so that another file of the same release does not pass.
	Name string
}

// ReleasePolicy is the policy for release tag of this repository.
func ReleasePolicy(tag string) Policy {
	return Policy{Owner: Owner, Repo: Repo, OtherNames: []string{LegacyRepo}, OwnerID: OwnerID, RepoID: RepoID,
		Workflow: Workflow, Tag: tag}
}

// SAN is the certificate subject the policy requires: the workflow file at
// the tag ref, in the repository with its current name.
func (p Policy) SAN() string { return p.sanFor(p.Repo) }

func (p Policy) sanFor(repo string) string {
	return "https://github.com/" + p.Owner + "/" + repo + "/" + p.Workflow + "@refs/tags/" + p.Tag
}

// validate checks that p is a complete policy for a release tag.
func (p Policy) validate() error {
	if !tagPattern.MatchString(p.Tag) {
		return fmt.Errorf("%w: %q is not a release tag", ErrPolicy, p.Tag)
	}
	if p.Owner == "" || p.Repo == "" || p.OwnerID == "" || p.RepoID == "" || !strings.HasPrefix(p.Workflow, ".github/workflows/") {
		return fmt.Errorf("%w: %+v", ErrPolicy, p)
	}
	return nil
}

// checkCertificate checks the subject and the Fulcio extensions of a
// signing certificate against p. Every field must match exactly.
func (p Policy) checkCertificate(san string, e fulcioExtensions) error {
	repo := ""
	for _, name := range append([]string{p.Repo}, p.OtherNames...) {
		if san == p.sanFor(name) {
			repo = name
		}
	}
	if repo == "" {
		return fmt.Errorf("certificate SAN %q, want %q", san, p.SAN())
	}
	repoURL := "https://github.com/" + p.Owner + "/" + repo
	for _, f := range []struct{ name, got, want string }{
		{"OIDC issuer", e.Issuer, Issuer},
		{"build config URI", e.BuildConfigURI, p.sanFor(repo)},
		{"runner environment", e.RunnerEnvironment, GitHubHosted},
		{"source repository URI", e.SourceRepositoryURI, repoURL},
		{"source repository identifier", e.SourceRepositoryIdentifier, p.RepoID},
		{"source repository owner URI", e.SourceRepositoryOwnerURI, "https://github.com/" + p.Owner},
		{"source repository owner identifier", e.SourceRepositoryOwnerIdentifier, p.OwnerID},
		{"source repository ref", e.SourceRepositoryRef, "refs/tags/" + p.Tag},
		{"build trigger", e.BuildTrigger, "push"},
	} {
		if f.got != f.want {
			return fmt.Errorf("certificate %s %q, want %q", f.name, f.got, f.want)
		}
	}
	if !commitPattern.MatchString(e.SourceRepositoryDigest) {
		return fmt.Errorf("certificate source repository digest %q is not a commit", e.SourceRepositoryDigest)
	}
	return nil
}

// Verifier fetches attestations from the GitHub API and verifies them.
type Verifier struct {
	// APIURL is DefaultAPIURL, or a test server.
	APIURL    string
	Client    *http.Client
	UserAgent string
	// TrustedRoot is the Sigstore trust material; nil means the
	// public-good instance's that is built into this binary.
	TrustedRoot *TrustedRoot
	// Now is the clock; nil means time.Now.
	Now func() time.Time
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
// GitHub API and accepts the first one whose Sigstore bundle verifies
// against the trusted root, whose certificate matches the policy, and
// whose statement has SLSA provenance v1 as its predicate and a subject
// with this digest (and the policy's file name, when set).
func (v *Verifier) Verify(ctx context.Context, digest []byte, p Policy) (*Result, error) {
	if len(digest) != 32 {
		return nil, fmt.Errorf("%w: a SHA-256 digest has 32 bytes, not %d", ErrPolicy, len(digest))
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	list, err := v.fetch(ctx, digest, p)
	if err != nil {
		return nil, err
	}
	tr := v.TrustedRoot
	if tr == nil {
		tr = PublicGoodRoot()
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	var errs []error
	untrusted := false
	for i, a := range list {
		raw, err := v.bundle(ctx, a)
		if err == nil {
			var res *Result
			if res, err = verifyOne(tr, raw, now(), digest, p); err == nil {
				return res, nil
			}
		}
		untrusted = untrusted || errors.Is(err, ErrUntrustedRoot)
		errs = append(errs, fmt.Errorf("attestation %d: %w", i+1, err))
	}
	err = fmt.Errorf("%w (sha256:%s, required signer %s): %w", ErrNotVerified, hex.EncodeToString(digest), p.SAN(), errors.Join(errs...))
	if untrusted {
		err = fmt.Errorf("%w\nThis binary's Sigstore trusted root (recorded %s) may be out of date. Download the file yourself, "+
			"check it with `gh attestation verify <file> --repo %s/%s`, which reads the current trusted root, and install it by hand",
			err, PublicGoodRootDate, p.Owner, p.Repo)
	}
	return nil, err
}

func verifyOne(tr *TrustedRoot, raw []byte, now time.Time, digest []byte, p Policy) (*Result, error) {
	v, err := tr.verifyBundle(raw, now)
	if err != nil {
		return nil, err
	}
	if err := p.checkCertificate(v.san, v.ext); err != nil {
		return nil, err
	}
	if err := checkStatement(&v.statement, digest, p.Name); err != nil {
		return nil, err
	}
	return &Result{SAN: v.san, Commit: v.ext.SourceRepositoryDigest}, nil
}

// checkStatement checks the predicate type of a verified statement, that
// a subject has the digest and, when name is set, that a subject of that
// name has the digest.
func checkStatement(st *statement, digest []byte, name string) error {
	if st == nil {
		return errors.New("no statement")
	}
	if st.PredicateType != PredicateSLSAv1 {
		return fmt.Errorf("predicate type %q, want %q", st.PredicateType, PredicateSLSAv1)
	}
	want := hex.EncodeToString(digest)
	found := false
	for _, sub := range st.Subject {
		if !strings.EqualFold(sub.Digest["sha256"], want) {
			continue
		}
		found = true
		if name == "" || sub.Name == name {
			return nil
		}
	}
	if !found {
		return fmt.Errorf("the artifact digest sha256:%s is not a subject of the statement", want)
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

func (v *Verifier) fetch(ctx context.Context, digest []byte, p Policy) ([]apiAttestation, error) {
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
	return all[:min(len(all), maxAttestations)], nil
}

// bundle reads the Sigstore bundle of an attestation: inline, or
// snappy-compressed JSON at bundle_url.
func (v *Verifier) bundle(ctx context.Context, a apiAttestation) ([]byte, error) {
	raw := []byte(a.Bundle)
	if len(raw) == 0 || string(raw) == jsonNull {
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
		if raw, err = decodeSnappy(data, maxBundle); err != nil {
			return nil, err
		}
	}
	if len(raw) > maxBundle {
		return nil, fmt.Errorf("bundle is larger than %d bytes", maxBundle)
	}
	return raw, nil
}
