// Package ocitest is a fake OCI registry for tests: one repository behind
// anonymous bearer tokens, over TLS.
package ocitest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/oci"
)

// Digest is the sha256 digest of b.
func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// Entry is one tar entry of a layer. Type 0 is a regular file and Mode 0
// is 0644.
type Entry struct {
	Name, Body, Link string
	Type             byte
	Mode             int64
}

// Layer is a gzip tar of entries.
func Layer(t testing.TB, entries ...Entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.Type
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.Mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.Name, Typeflag: typ, Mode: mode, Linkname: e.Link, ModTime: time.Unix(1_700_000_000, 0)}
		if typ == tar.TypeReg {
			hdr.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Registry serves one repository.
type Registry struct {
	Server *httptest.Server
	Repo   string

	mu     sync.Mutex
	blobs  map[string][]byte
	served map[string][]byte
	// Basic makes every request but the token one fail with a Basic
	// challenge, as a registry that wants credentials does.
	basic    bool
	stall    string
	tokens   int
	requests []string
}

// Stall makes the registry send the headers and part of digest's body,
// then nothing until the request ends.
func (r *Registry) Stall(digest string) {
	r.mu.Lock()
	r.stall = digest
	r.mu.Unlock()
}

// New starts a registry for repo.
func New(t testing.TB, repo string) *Registry {
	t.Helper()
	r := &Registry{Repo: repo, blobs: map[string][]byte{}, served: map[string][]byte{}}
	r.Server = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Server.Close)
	return r
}

// Domain is the registry's host:port.
func (r *Registry) Domain() string { return strings.TrimPrefix(r.Server.URL, "https://") }

// Client trusts the registry's certificate.
func (r *Registry) Client() *http.Client { return r.Server.Client() }

// RequireBasic makes the registry want credentials.
func (r *Registry) RequireBasic() {
	r.mu.Lock()
	r.basic = true
	r.mu.Unlock()
}

// Add stores b and returns its digest.
func (r *Registry) Add(b []byte) string {
	d := Digest(b)
	r.mu.Lock()
	r.blobs[d] = b
	r.mu.Unlock()
	return d
}

// Tamper makes the registry serve body for digest.
func (r *Registry) Tamper(digest string, body []byte) {
	r.mu.Lock()
	r.served[digest] = body
	r.mu.Unlock()
}

// Tokens is the number of tokens handed out.
func (r *Registry) Tokens() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tokens
}

// Requests counts the requests whose path has the prefix.
func (r *Registry) Requests(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, p := range r.requests {
		if strings.HasPrefix(p, prefix) {
			n++
		}
	}
	return n
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req.URL.Path)
	if req.URL.Path == "/token" {
		if req.URL.Query().Get("scope") != "repository:"+r.Repo+":pull" || req.URL.Query().Get("service") != "fake" {
			http.Error(w, "bad scope", http.StatusBadRequest)
			return
		}
		r.tokens++
		_, _ = w.Write([]byte(`{"token":"good"}`))
		return
	}
	if r.basic {
		w.Header().Set("WWW-Authenticate", `Basic realm="Authorization Required"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if req.Header.Get("Authorization") != "Bearer good" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+r.Server.URL+`/token",service="fake",scope="repository:`+r.Repo+`:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/"+r.Repo+"/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	_, digest, _ := strings.Cut(rest, "/")
	body, ok := r.served[digest]
	if !ok {
		body, ok = r.blobs[digest]
	}
	if !ok {
		http.NotFound(w, req)
		return
	}
	if digest == r.stall {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body[:len(body)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		r.mu.Unlock()
		<-req.Context().Done()
		r.mu.Lock()
		return
	}
	if strings.HasPrefix(rest, "manifests/") {
		var m struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(body, &m)
		w.Header().Set("Content-Type", m.MediaType)
	}
	_, _ = w.Write(body)
}

// Image is the digests of a pushed image.
type Image struct {
	Index, Manifest, Config string
	Layers                  []string
}

type config struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Config       struct {
		Env []string `json:"Env"`
	} `json:"config"`
}

type manifest struct {
	SchemaVersion int              `json:"schemaVersion"`
	MediaType     string           `json:"mediaType"`
	Config        *oci.Descriptor  `json:"config,omitempty"`
	Layers        []oci.Descriptor `json:"layers,omitempty"`
	Manifests     []oci.Descriptor `json:"manifests,omitempty"`
}

func (r *Registry) addJSON(t testing.TB, v any) (string, int64) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return r.Add(b), int64(len(b))
}

// Push stores a linux/amd64 image with env and layers of mediaType, behind
// an index that also lists arm64 and an attestation.
func (r *Registry) Push(t testing.TB, mediaType string, env []string, layers ...[]byte) Image {
	t.Helper()
	cfg := config{OS: "linux", Architecture: "amd64"}
	cfg.Config.Env = env
	var img Image
	var cfgSize int64
	img.Config, cfgSize = r.addJSON(t, cfg)
	man := manifest{SchemaVersion: 2, MediaType: oci.MediaOCIManifest,
		Config: &oci.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: img.Config, Size: cfgSize}}
	for _, l := range layers {
		d := r.Add(l)
		img.Layers = append(img.Layers, d)
		man.Layers = append(man.Layers, oci.Descriptor{MediaType: mediaType, Digest: d, Size: int64(len(l))})
	}
	var manSize int64
	img.Manifest, manSize = r.addJSON(t, man)
	other, otherSize := r.addJSON(t, manifest{SchemaVersion: 2, MediaType: oci.MediaOCIManifest})
	desc := func(digest string, size int64, os, arch string) oci.Descriptor {
		return oci.Descriptor{MediaType: oci.MediaOCIManifest, Digest: digest, Size: size, Platform: &oci.Platform{OS: os, Architecture: arch}}
	}
	img.Index, _ = r.addJSON(t, manifest{SchemaVersion: 2, MediaType: oci.MediaOCIIndex, Manifests: []oci.Descriptor{
		desc(other, otherSize, "unknown", "unknown"), desc(other, otherSize, "linux", "arm64"),
		desc(img.Manifest, manSize, "linux", "amd64"),
	}})
	return img
}

// Reference is the registry's reference to digest with tag v1.
func (r *Registry) Reference(digest string) string {
	return r.Domain() + "/" + r.Repo + ":v1@" + digest
}
