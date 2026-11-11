package attest_test

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/hermes-device/internal/attest"
	"github.com/sigstore/sigstore-go/pkg/root"
)

// Recorded on 2026-11-11 from the GitHub API: the attestations of
// goreleaser_Linux_x86_64.tar.gz of goreleaser/goreleaser v2.20.1, signed by
// its release workflow at the tag, and of gh_2.103.3_linux_amd64.tar.gz of
// cli/cli, signed by its deployment workflow on the trunk branch, whose API
// answer has only bundle URLs. The trusted roots are `gh attestation
// trusted-root` of the same day.
const (
	goreleaserDigest = "79e0589ca9efd70e8db85158a21334cbb57811d4034b39b61d55789f361341a5"
	ghDigest         = "4c806978ffce1cec70853302a56d70ac6da4f19bae7adca4e1a8ab00633dd91d"
)

func goreleaserPolicy() attest.Policy {
	return attest.Policy{
		Owner: "goreleaser", Repo: "goreleaser", OwnerID: "24697112", RepoID: "77071454",
		Workflow: ".github/workflows/release.yml", Tag: "v2.20.1", Name: "goreleaser_Linux_x86_64.tar.gz",
	}
}

func ghPolicy() attest.Policy {
	return attest.Policy{
		Owner: "cli", Repo: "cli", OwnerID: "59704711", RepoID: "212613049",
		Workflow: ".github/workflows/deployment.yml", Tag: "v2.103.3",
	}
}

func digest(t *testing.T, s string) []byte {
	t.Helper()
	d, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func trustedRoot(t *testing.T, name string) func(context.Context) (root.TrustedMaterial, error) {
	t.Helper()
	tr, err := root.NewTrustedRootFromJSON(readTestdata(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return func(context.Context) (root.TrustedMaterial, error) { return tr, nil }
}

// apiServer answers every attestation request with the recorded answer
// for the given digest, the way a hostile API or mirror could, and 404
// otherwise; body "" answers 404 to all.
func apiServer(t *testing.T, recorded, body string) *attest.Verifier {
	t.Helper()
	bundles := map[string][]byte{
		"/bundles/60065904.json.sn": readTestdata(t, "60065904.json.sn"),
		"/bundles/60066569.json.sn": readTestdata(t, "60066569.json.sn"),
	}
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case bundles[r.URL.Path] != nil:
			_, _ = w.Write(bundles[r.URL.Path])
		case body != "" && strings.Contains(r.URL.Path, "/attestations/sha256:"):
			if r.URL.Query().Get("predicate_type") != attest.PredicateSLSAv1 {
				t.Errorf("query %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(strings.ReplaceAll(body, "BUNDLE_URL", srv.URL+"/bundles")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &attest.Verifier{APIURL: srv.URL, Client: srv.Client(), TrustedRoot: trustedRoot(t, recorded)}
}

func TestVerifyRecordedRelease(t *testing.T) {
	t.Parallel()
	v := apiServer(t, "trusted_root_public_good.json", string(readTestdata(t, "goreleaser_v2.20.1.json")))
	res, err := v.Verify(t.Context(), digest(t, goreleaserDigest), goreleaserPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if res.SAN != "https://github.com/goreleaser/goreleaser/.github/workflows/release.yml@refs/tags/v2.20.1" || res.Commit != "119f88f695dc34e4b6f04c34e22005955d5675f8" {
		t.Errorf("result %+v", res)
	}
}

func TestVerifyRefusals(t *testing.T) {
	t.Parallel()
	goreleaser := string(readTestdata(t, "goreleaser_v2.20.1.json"))
	gh := string(readTestdata(t, "gh_v2.103.3.json"))
	with := func(f func(*attest.Policy)) attest.Policy {
		p := goreleaserPolicy()
		f(&p)
		return p
	}
	for name, tc := range map[string]struct {
		body     string
		root     string
		digest   string
		policy   attest.Policy
		want     error
		wantText string
	}{
		"wrong repo":       {body: goreleaser, policy: with(func(p *attest.Policy) { p.Repo = "nfpm" }), want: attest.ErrNotVerified, wantText: `expected SAN value "https://github.com/goreleaser/nfpm/`},
		"wrong owner":      {body: goreleaser, policy: with(func(p *attest.Policy) { p.Owner = "goreleaser-pro" }), want: attest.ErrNotVerified, wantText: `expected SAN value "https://github.com/goreleaser-pro/`},
		"wrong file name":  {body: goreleaser, policy: with(func(p *attest.Policy) { p.Name = "goreleaser_Linux_arm64.tar.gz" }), want: attest.ErrNotVerified, wantText: `no subject named "goreleaser_Linux_arm64.tar.gz"`},
		"wrong repo id":    {body: goreleaser, policy: with(func(p *attest.Policy) { p.RepoID = "1" }), want: attest.ErrNotVerified, wantText: "SourceRepositoryIdentifier"},
		"wrong owner id":   {body: goreleaser, policy: with(func(p *attest.Policy) { p.OwnerID = "1" }), want: attest.ErrNotVerified, wantText: "SourceRepositoryOwnerIdentifier"},
		"wrong workflow":   {body: goreleaser, policy: with(func(p *attest.Policy) { p.Workflow = ".github/workflows/build.yml" }), want: attest.ErrNotVerified, wantText: "expected SAN value"},
		"other tag":        {body: goreleaser, policy: with(func(p *attest.Policy) { p.Tag = "v2.20.0" }), want: attest.ErrNotVerified, wantText: `release.yml@refs/tags/v2.20.0", got`},
		"tampered digest":  {body: goreleaser, digest: ghDigest, policy: goreleaserPolicy(), want: attest.ErrNotVerified, wantText: "artifact digest"},
		"untrusted root":   {body: goreleaser, root: "trusted_root_github.json", policy: goreleaserPolicy(), want: attest.ErrNotVerified, wantText: "transparency log"},
		"branch ref":       {body: gh, digest: ghDigest, policy: ghPolicy(), want: attest.ErrNotVerified, wantText: "refs/heads/trunk"},
		"no attestation":   {policy: goreleaserPolicy(), want: attest.ErrNoAttestation},
		"empty list":       {body: `{"attestations":[]}`, policy: goreleaserPolicy(), want: attest.ErrNoAttestation},
		"garbage bundle":   {body: `{"attestations":[{"bundle":{"mediaType":"x"}}]}`, policy: goreleaserPolicy(), want: attest.ErrNotVerified},
		"http bundle URL":  {body: `{"attestations":[{"bundle":null,"bundle_url":"http://example.com/b"}]}`, policy: goreleaserPolicy(), want: attest.ErrNotVerified},
		"tag not semver":   {body: goreleaser, policy: with(func(p *attest.Policy) { p.Tag = "main" }), want: attest.ErrPolicy},
		"tag with a ref":   {body: goreleaser, policy: with(func(p *attest.Policy) { p.Tag = "v2.20.1@refs/heads/main" }), want: attest.ErrPolicy},
		"workflow outside": {body: goreleaser, policy: with(func(p *attest.Policy) { p.Workflow = "release.yml" }), want: attest.ErrPolicy},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := tc.root
			if r == "" {
				r = "trusted_root_public_good.json"
			}
			d := tc.digest
			if d == "" {
				d = goreleaserDigest
			}
			res, err := apiServer(t, r, tc.body).Verify(t.Context(), digest(t, d), tc.policy)
			if !errors.Is(err, tc.want) || tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("Verify = %+v, %v", res, err)
			}
		})
	}
}

func TestVerifyNeedsSHA256(t *testing.T) {
	t.Parallel()
	v := apiServer(t, "trusted_root_public_good.json", "")
	if _, err := v.Verify(t.Context(), []byte{1, 2}, goreleaserPolicy()); !errors.Is(err, attest.ErrPolicy) {
		t.Fatal(err)
	}
}

func TestReleasePolicy(t *testing.T) {
	t.Parallel()
	if got := attest.ReleasePolicy("v1.2.3").SAN(); got != "https://github.com/USA-RedDragon/hermes-device/.github/workflows/release.yaml@refs/tags/v1.2.3" {
		t.Errorf("SAN = %q", got)
	}
}

func TestVerifyFollowsPages(t *testing.T) {
	t.Parallel()
	page2 := readTestdata(t, "goreleaser_v2.20.1.json")
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write(page2)
			return
		}
		w.Header().Set("Link", "<"+srv.URL+r.URL.Path+"?page=2>; rel=\"next\", <"+srv.URL+r.URL.Path+"?page=2>; rel=\"last\"")
		_, _ = w.Write([]byte(`{"attestations":[{"bundle":{"mediaType":"x"}}]}`))
	}))
	t.Cleanup(srv.Close)
	v := &attest.Verifier{APIURL: srv.URL, Client: srv.Client(), TrustedRoot: trustedRoot(t, "trusted_root_public_good.json")}
	if _, err := v.Verify(t.Context(), digest(t, goreleaserDigest), goreleaserPolicy()); err != nil {
		t.Fatal(err)
	}
}
