package oci

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// Media types of manifests and layers.
const (
	MediaOCIIndex        = "application/vnd.oci.image.index.v1+json"
	MediaOCIManifest     = "application/vnd.oci.image.manifest.v1+json"
	MediaDockerList      = "application/vnd.docker.distribution.manifest.list.v2+json"
	MediaDockerManifest  = "application/vnd.docker.distribution.manifest.v2+json"
	MediaOCILayerGzip    = "application/vnd.oci.image.layer.v1.tar+gzip"
	MediaOCILayer        = "application/vnd.oci.image.layer.v1.tar"
	MediaDockerLayerGzip = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	MediaDockerLayer     = "application/vnd.docker.image.rootfs.diff.tar"
)

// Limits of a pull.
const (
	MaxManifestSize    = 4 << 20
	MaxConfigSize      = 8 << 20
	MaxLayers          = 512
	MaxLayerSize       = 8 << 30
	DefaultMaxUnpacked = 16 << 30
	MaxEntries         = 4 << 20
)

// Pull errors.
var (
	ErrPlatform   = errors.New("image has no build for this platform")
	ErrMediaType  = errors.New("unsupported media type")
	ErrUnsafePath = errors.New("layer entry would land outside the target directory")
)

const acceptManifests = MediaOCIIndex + ", " + MediaDockerList + ", " + MediaOCIManifest + ", " + MediaDockerManifest

// Options configure Pull.
type Options struct {
	// HTTP is the client for the registry; nil uses one without a total
	// timeout. Every request fails after IdleTimeout without data.
	// Zero means DefaultIdleTimeout.
	IdleTimeout time.Duration
	HTTP        *http.Client
	UserAgent   string
	// OS and Arch select the image of an index. Empty means linux and
	// runtime.GOARCH.
	OS, Arch string
	// Filter selects the files to extract.
	Filter Filter
	// Upstreams are the registries a mirror path may fall back to
	// (Reference.Upstream). Nil means DefaultUpstreams.
	Upstreams []string
	// MaxUnpacked caps the bytes written. Zero means DefaultMaxUnpacked.
	MaxUnpacked int64
	// Progress, when set, is called after each layer with the compressed
	// bytes done and in total.
	Progress func(done, total int64)
	Log      *slog.Logger
}

// Descriptor points at a blob or manifest.
type Descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Platform  *Platform `json:"platform,omitempty"`
	URLs      []string  `json:"urls,omitempty"`
}

// Platform of an index entry.
type Platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

type manifest struct {
	MediaType string       `json:"mediaType"`
	Manifests []Descriptor `json:"manifests"`
	Config    *Descriptor  `json:"config"`
	Layers    []Descriptor `json:"layers"`
}

// Config is the part of the image config Pull returns.
type Config struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Config       struct {
		Env        []string `json:"Env"`
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
		WorkingDir string   `json:"WorkingDir"`
	} `json:"config"`
}

// Image is what Pull extracted.
type Image struct {
	// Source is the reference that served the image: the requested one,
	// or its Upstream.
	Source Reference
	// Manifest is the digest of the platform's manifest.
	Manifest string
	Config   Config
}

// Pull extracts the files of ref's image for the platform that opts.Filter
// selects into dest, an existing empty directory. When ref's registry
// fails before the layers and ref is a mirror path (Reference.Upstream),
// it pulls the same digest from the upstream registry. On an error dest
// holds a partial tree that the caller must remove; nothing in it was
// verified.
func Pull(ctx context.Context, ref Reference, dest string, opts Options) (*Image, error) {
	if opts.HTTP == nil {
		opts.HTTP = &http.Client{}
	}
	if opts.Upstreams == nil {
		opts.Upstreams = DefaultUpstreams
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.OS == "" {
		opts.OS = "linux"
	}
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	reg := &registry{http: opts.HTTP, ref: ref, ua: opts.UserAgent, idle: opts.IdleTimeout}
	m, mdigest, cfg, err := resolve(ctx, reg, opts)
	if up, ok := ref.Upstream(opts.Upstreams); err != nil && ok && ctx.Err() == nil {
		log.Warn("Registry failed; pulling the same digest from the upstream registry", "registry", ref.Domain,
			"upstream", up.Domain, "error", err)
		reg = &registry{http: opts.HTTP, ref: up, ua: opts.UserAgent, idle: opts.IdleTimeout}
		var uerr error
		if m, mdigest, cfg, uerr = resolve(ctx, reg, opts); uerr != nil {
			return nil, errors.Join(err, uerr)
		}
		err = nil
	}
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	limit := opts.MaxUnpacked
	if limit <= 0 {
		limit = DefaultMaxUnpacked
	}
	x := &extractor{root: root, filter: opts.Filter, log: log, budget: limit}
	var total, done int64
	for _, l := range m.Layers {
		total += l.Size
	}
	for i, l := range m.Layers {
		if err := pullLayer(ctx, reg, l, x); err != nil {
			return nil, fmt.Errorf("layer %d (%s): %w", i, l.Digest, err)
		}
		done += l.Size
		if opts.Progress != nil {
			opts.Progress(done, total)
		}
	}
	return &Image{Source: reg.ref, Manifest: mdigest, Config: *cfg}, nil
}

// resolve fetches and checks the manifest for the platform and its
// config.
func resolve(ctx context.Context, reg *registry, opts Options) (*manifest, string, *Config, error) {
	m, mt, err := fetchManifest(ctx, reg, reg.ref.Digest, -1)
	if err != nil {
		return nil, "", nil, err
	}
	digest := reg.ref.Digest
	if mt == MediaOCIIndex || mt == MediaDockerList {
		d, err := pickPlatform(m.Manifests, opts.OS, opts.Arch)
		if err != nil {
			return nil, "", nil, err
		}
		if d.MediaType != MediaOCIManifest && d.MediaType != MediaDockerManifest {
			return nil, "", nil, fmt.Errorf("%w: manifest %q", ErrMediaType, d.MediaType)
		}
		if m, mt, err = fetchManifest(ctx, reg, d.Digest, d.Size); err != nil {
			return nil, "", nil, err
		}
		digest = d.Digest
	}
	if mt != MediaOCIManifest && mt != MediaDockerManifest {
		return nil, "", nil, fmt.Errorf("%w: manifest %q", ErrMediaType, mt)
	}
	if m.Config == nil || len(m.Layers) == 0 || len(m.Layers) > MaxLayers {
		return nil, "", nil, fmt.Errorf("manifest %s has no config or %d layers", digest, len(m.Layers))
	}
	for _, l := range m.Layers {
		if err := checkDescriptor(l, MaxLayerSize); err != nil {
			return nil, "", nil, err
		}
		if len(l.URLs) > 0 {
			return nil, "", nil, fmt.Errorf("%w: layer %s has foreign URLs", ErrMediaType, l.Digest)
		}
		if !supportedLayer(l.MediaType) {
			return nil, "", nil, fmt.Errorf("%w: layer %q", ErrMediaType, l.MediaType)
		}
	}
	if err := checkDescriptor(*m.Config, MaxConfigSize); err != nil {
		return nil, "", nil, err
	}
	raw, err := fetchBlob(ctx, reg, *m.Config)
	if err != nil {
		return nil, "", nil, fmt.Errorf("config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, "", nil, fmt.Errorf("config: %w", err)
	}
	if cfg.OS != opts.OS || cfg.Architecture != opts.Arch {
		return nil, "", nil, fmt.Errorf("%w: %s/%s, the image is %s/%s", ErrPlatform, opts.OS, opts.Arch, cfg.OS, cfg.Architecture)
	}
	return m, digest, &cfg, nil
}

func checkDescriptor(d Descriptor, limit int64) error {
	if !digestRE.MatchString(d.Digest) {
		return fmt.Errorf("%w: digest %q", ErrBadReference, d.Digest)
	}
	if d.Size <= 0 || d.Size > limit {
		return fmt.Errorf("%s: size %d: %w", d.Digest, d.Size, ErrTooLarge)
	}
	return nil
}

func pickPlatform(list []Descriptor, goos, arch string) (Descriptor, error) {
	for _, d := range list {
		p := d.Platform
		if p == nil || p.OS != goos || p.Architecture != arch {
			continue
		}
		if arch == "arm64" && p.Variant != "" && p.Variant != "v8" {
			continue
		}
		return d, nil
	}
	return Descriptor{}, fmt.Errorf("%w: %s/%s", ErrPlatform, goos, arch)
}

// fetchManifest gets the manifest with digest and checks its bytes
// against it. size is the expected size, or -1 when unknown.
func fetchManifest(ctx context.Context, reg *registry, digest string, size int64) (*manifest, string, error) {
	resp, err := reg.get(ctx, "/manifests/"+digest, acceptManifests)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxManifestSize+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > MaxManifestSize {
		return nil, "", fmt.Errorf("manifest %s: %w", digest, ErrTooLarge)
	}
	if got := sha256Digest(body); got != digest || (size >= 0 && int64(len(body)) != size) {
		return nil, "", fmt.Errorf("manifest %s: %w (got %s, %d bytes)", digest, ErrDigest, got, len(body))
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", fmt.Errorf("manifest %s: %w", digest, err)
	}
	mt := m.MediaType
	if mt == "" {
		mt, _, _ = strings.Cut(resp.Header.Get("Content-Type"), ";")
		mt = strings.TrimSpace(mt)
	}
	return &m, mt, nil
}

func sha256Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// fetchBlob reads a small blob whole and checks it.
func fetchBlob(ctx context.Context, reg *registry, d Descriptor) ([]byte, error) {
	resp, err := reg.get(ctx, "/blobs/"+d.Digest, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, d.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != d.Size || sha256Digest(body) != d.Digest {
		return nil, fmt.Errorf("blob %s: %w", d.Digest, ErrDigest)
	}
	return body, nil
}

// verifier hashes and counts what passes through it.
type verifier struct {
	r io.Reader
	h hash.Hash
	n int64
}

func (v *verifier) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	v.h.Write(p[:n])
	v.n += int64(n)
	return n, err
}

// pullLayer streams one layer into x, hashing the compressed bytes, and
// fails unless they match the descriptor exactly.
func pullLayer(ctx context.Context, reg *registry, d Descriptor, x *extractor) error {
	resp, err := reg.get(ctx, "/blobs/"+d.Digest, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	v := &verifier{r: io.LimitReader(resp.Body, d.Size+1), h: sha256.New()}
	tr, err := layerReader(d.MediaType, v)
	if err == nil {
		err = x.layer(tr)
	}
	// The rest of the blob is hashed even after an error, so that bytes
	// that are not the layer report as such rather than as a bad archive.
	if _, cerr := io.Copy(io.Discard, v); cerr != nil {
		return errors.Join(err, cerr)
	}
	if got := "sha256:" + hex.EncodeToString(v.h.Sum(nil)); v.n != d.Size || got != d.Digest {
		return fmt.Errorf("%w: got %s, %d bytes", ErrDigest, got, v.n)
	}
	return err
}

// layerReader returns the tar stream of a layer with mediaType.
func layerReader(mediaType string, r io.Reader) (io.Reader, error) {
	switch mediaType {
	case MediaOCILayerGzip, MediaDockerLayerGzip:
		return gzip.NewReader(r)
	case MediaOCILayer, MediaDockerLayer:
		return r, nil
	}
	return nil, fmt.Errorf("%w: layer %q", ErrMediaType, mediaType)
}

// supportedLayer reports whether layerReader reads mediaType. zstd layers
// are not supported: the standard library has no zstd reader.
func supportedLayer(mediaType string) bool {
	switch mediaType {
	case MediaOCILayerGzip, MediaDockerLayerGzip, MediaOCILayer, MediaDockerLayer:
		return true
	}
	return false
}
