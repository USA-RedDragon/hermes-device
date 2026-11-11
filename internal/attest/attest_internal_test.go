package attest

import (
	"strings"
	"testing"

	in_toto "github.com/in-toto/attestation/go/v1"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
)

const fileA = "a.tar.gz"

func TestCheckStatement(t *testing.T) {
	t.Parallel()
	d := make([]byte, 32)
	sum := strings.Repeat("00", 32)
	st := func(pt, name, sha string) *in_toto.Statement {
		return &in_toto.Statement{PredicateType: pt, Subject: []*in_toto.ResourceDescriptor{{Name: name, Digest: map[string]string{"sha256": sha}}}}
	}
	for name, tc := range map[string]struct {
		st   *in_toto.Statement
		file string
		ok   bool
	}{
		"slsa":            {st: st(PredicateSLSAv1, fileA, sum), file: fileA, ok: true},
		"any name":        {st: st(PredicateSLSAv1, fileA, sum), ok: true},
		"sbom":            {st: st("https://spdx.dev/Document/v2.3", fileA, sum), file: fileA},
		"no statement":    {file: fileA},
		"other name":      {st: st(PredicateSLSAv1, "b.tar.gz", sum), file: fileA},
		"name, other sum": {st: st(PredicateSLSAv1, fileA, strings.Repeat("11", 32)), file: fileA},
	} {
		if err := checkStatement(tc.st, d, tc.file); (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestPolicyRequiresHostedRunnerAndPush(t *testing.T) {
	t.Parallel()
	id, err := ReleasePolicy("v1.2.3").identity()
	if err != nil {
		t.Fatal(err)
	}
	e := id.Extensions
	if e.RunnerEnvironment != GitHubHosted || e.BuildTrigger != "push" || e.SourceRepositoryRef != "refs/tags/v1.2.3" ||
		e.SourceRepositoryIdentifier != RepoID || e.SourceRepositoryOwnerIdentifier != OwnerID || e.BuildConfigURI != ReleasePolicy("v1.2.3").SAN() {
		t.Errorf("extensions %+v", e)
	}
	got := e
	got.RunnerEnvironment = "self-hosted"
	if err := id.Verify(summaryWith(ReleasePolicy("v1.2.3").SAN(), got)); err == nil {
		t.Error("accepted a self-hosted runner")
	}
	got = e
	got.BuildTrigger = "workflow_dispatch"
	if err := id.Verify(summaryWith(ReleasePolicy("v1.2.3").SAN(), got)); err == nil {
		t.Error("accepted a workflow_dispatch run")
	}
	got = e
	got.Issuer = "https://example.com"
	if err := id.Verify(summaryWith(ReleasePolicy("v1.2.3").SAN(), got)); err == nil {
		t.Error("accepted another issuer")
	}
	if err := id.Verify(summaryWith(ReleasePolicy("v1.2.3").SAN(), e)); err != nil {
		t.Errorf("refused the matching certificate: %v", err)
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

func summaryWith(san string, e certificate.Extensions) certificate.Summary {
	if e.Issuer == "" {
		e.Issuer = Issuer
	}
	return certificate.Summary{SubjectAlternativeName: san, Extensions: e}
}
