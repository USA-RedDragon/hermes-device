// Package oci pulls one platform of a container image from an OCI or
// Docker registry by digest and extracts the files it needs, with the Go
// standard library only. Every manifest and blob is checked against its
// digest while it streams, so the content is exactly what the digest
// names.
package oci

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// DockerHub is the domain of Docker Hub in references, and dockerHubHost
// the host of its registry API.
const (
	DockerHub     = "docker.io"
	dockerHubHost = "registry-1.docker.io"
)

// Reference errors.
var (
	ErrBadReference = errors.New("invalid image reference")
	ErrNoDigest     = errors.New("image reference has no @sha256: digest")
)

// Reference is an image reference pinned to a digest.
type Reference struct {
	// Domain is the registry, such as docker.io or registry.example.com:5000.
	Domain string
	// Repository is the path in the registry, such as library/alpine.
	Repository string
	// Tag is informational only; the digest selects the content.
	Tag string
	// Digest is "sha256:" and 64 lowercase hex digits.
	Digest string
}

var (
	repoRE   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagRE    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	domainRE = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?)*(?::[0-9]+)?$`)
	digestRE = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// ParseReference parses [domain/]repository[:tag]@sha256:<hex>. A
// reference without a digest is refused with ErrNoDigest.
func ParseReference(s string) (Reference, error) {
	name, digest, ok := strings.Cut(strings.TrimSpace(s), "@")
	if !ok {
		return Reference{}, fmt.Errorf("%w: %q", ErrNoDigest, s)
	}
	if !digestRE.MatchString(digest) {
		return Reference{}, fmt.Errorf("%w: %q: the digest must be sha256: and 64 lowercase hex digits", ErrBadReference, s)
	}
	var tag string
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name, tag = name[:i], name[i+1:]
		if !tagRE.MatchString(tag) {
			return Reference{}, fmt.Errorf("%w: %q: bad tag", ErrBadReference, s)
		}
	}
	domain, repo := splitDomain(name)
	if !domainRE.MatchString(domain) || !repoRE.MatchString(repo) {
		return Reference{}, fmt.Errorf("%w: %q", ErrBadReference, s)
	}
	return Reference{Domain: domain, Repository: repo, Tag: tag, Digest: digest}, nil
}

// splitDomain splits name the way Docker does: the first component is a
// registry when it has a dot or a port, or is localhost.
func splitDomain(name string) (domain, repo string) {
	first, rest, ok := strings.Cut(name, "/")
	if !ok || !looksLikeDomain(first) {
		domain, repo = DockerHub, name
	} else {
		domain, repo = first, rest
	}
	if domain == "index.docker.io" {
		domain = DockerHub
	}
	if domain == DockerHub && !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	return domain, repo
}

func looksLikeDomain(s string) bool {
	return strings.ContainsAny(s, ".:") || s == "localhost"
}

// String is the reference in its usual form.
func (r Reference) String() string {
	s := r.Domain + "/" + r.Repository
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	return s + "@" + r.Digest
}

// Hex is the digest without its "sha256:" prefix.
func (r Reference) Hex() string { return strings.TrimPrefix(r.Digest, "sha256:") }

// DefaultUpstreams are the public registries that a mirror path may name.
var DefaultUpstreams = []string{ //nolint:gochecknoglobals // fixed table
	DockerHub, "index.docker.io", "ghcr.io", "quay.io", "gcr.io", "registry.k8s.io", "public.ecr.aws",
	"mcr.microsoft.com", "registry.gitlab.com", "nvcr.io",
}

// Upstream is the reference a pull-through mirror proxies, for a mirror
// path of the form <mirror>/<upstream>/<repository> where upstream is one
// of upstreams, such as registry.example.com/docker.io/library/alpine. ok
// is false for any other path, so a repository path whose first part only
// looks like a host is never contacted. The digest pins the content, so
// the upstream serves the same bytes or the pull fails.
func (r Reference) Upstream(upstreams []string) (up Reference, ok bool) {
	first, rest, found := strings.Cut(r.Repository, "/")
	if !found || !slices.Contains(upstreams, first) || first == r.Domain {
		return Reference{}, false
	}
	domain, repo := splitDomain(first + "/" + rest)
	return Reference{Domain: domain, Repository: repo, Tag: r.Tag, Digest: r.Digest}, true
}

func (r Reference) host() string {
	if r.Domain == DockerHub {
		return dockerHubHost
	}
	return r.Domain
}
