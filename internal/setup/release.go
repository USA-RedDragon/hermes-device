package setup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// DefaultReleasesURL is the repository's releases page.
const DefaultReleasesURL = "https://github.com/USA-RedDragon/hermes-device/releases"

// Download limits.
const (
	maxArchive   = 256 << 20
	maxChecksums = 1 << 20
)

// Release errors.
var (
	ErrChecksum = errors.New("checksum mismatch")
	ErrNoAsset  = errors.New("the release has no such file")
)

// Releases downloads release files from GitHub.
type Releases struct {
	// BaseURL is DefaultReleasesURL, or a test server.
	BaseURL   string
	Client    *http.Client
	UserAgent string
}

func (r *Releases) get(ctx context.Context, u string, client *http.Client) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}
	return client.Do(req)
}

// LatestTag returns the tag of the latest release, from the redirect of
// <BaseURL>/latest. It uses no API, so no rate limit applies.
func (r *Releases) LatestTag(ctx context.Context) (string, error) {
	c := *r.Client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := r.get(ctx, r.BaseURL+"/latest", &c)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", fmt.Errorf("%s/latest answered %s, not a redirect to the latest release", r.BaseURL, resp.Status)
	}
	loc, err := resp.Location()
	if err != nil {
		return "", err
	}
	dir, tag := path.Split(loc.Path)
	if !strings.HasSuffix(dir, "/releases/tag/") || Canonical(tag) == "" {
		return "", fmt.Errorf("the latest release redirects to %s, which names no release tag", loc)
	}
	return tag, nil
}

// AssetURL is the download URL of file name of the release tag.
func (r *Releases) AssetURL(tag, name string) string {
	return r.BaseURL + "/download/" + url.PathEscape(tag) + "/" + url.PathEscape(name)
}

// Fetch downloads file name of the release tag, up to limit bytes.
func (r *Releases) Fetch(ctx context.Context, tag, name string, limit int64) ([]byte, error) {
	u := r.AssetURL(tag, name)
	resp, err := r.get(ctx, u, r.Client)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNoAsset, u)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", u, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", u, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download %s: larger than %d bytes", u, limit)
	}
	return data, nil
}

// ArchiveName is the release archive of version (without v) for an OS and
// architecture. goarm is the ARM version (6 or 7) for goarch arm.
func ArchiveName(version, goos, goarch, goarm string) string {
	arch := goarch
	if goarch == "arm" {
		arch = "armv" + goarm
	}
	return fmt.Sprintf("%s_%s_%s_%s.tar.gz", BinaryName, version, goos, arch)
}

// ChecksumsName is the release's checksum file of version (without v).
func ChecksumsName(version string) string {
	return fmt.Sprintf("%s_%s_checksums.txt", BinaryName, version)
}

// ParseChecksums reads a sha256sum file: "<hex>  <name>" lines.
func ParseChecksums(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && len(f[0]) == 64 {
			if _, err := hex.DecodeString(f[0]); err == nil {
				out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
			}
		}
	}
	return out
}

// ExtractBinary returns the hermes-device file at the top of a tar.gz
// release archive.
func ExtractBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("the archive has no %s", BinaryName)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Clean(h.Name) == BinaryName {
			if h.Size > maxArchive {
				return nil, fmt.Errorf("%s in the archive is too large", BinaryName)
			}
			return io.ReadAll(io.LimitReader(tr, maxArchive))
		}
	}
}

// FetchArchive downloads the release archive of tag for an OS and
// architecture, checks it against the release's checksum file and returns
// it with its SHA-256 digest.
func (r *Releases) FetchArchive(ctx context.Context, tag, goos, goarch, goarm string) ([]byte, []byte, error) {
	v := strings.TrimPrefix(tag, "v")
	sums, err := r.Fetch(ctx, tag, ChecksumsName(v), maxChecksums)
	if err != nil {
		return nil, nil, err
	}
	name := ArchiveName(v, goos, goarch, goarm)
	want, ok := ParseChecksums(sums)[name]
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s lists no %s (no build for %s/%s?)", ErrNoAsset, ChecksumsName(v), name, goos, goarch)
	}
	archive, err := r.Fetch(ctx, tag, name, maxArchive)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, nil, fmt.Errorf("%w: %s has sha256 %s, the checksum file says %s", ErrChecksum, name, got, want)
	}
	return archive, sum[:], nil
}

// XPIName is the signed Firefox extension file of version (without v).
func XPIName(version string) string { return "hermes-device-firefox-" + version + ".xpi" }

// Has reports whether release tag has file name.
func (r *Releases) Has(ctx context.Context, tag, name string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, r.AssetURL(tag, name), nil)
	if err != nil {
		return false, err
	}
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("HEAD %s: %s", r.AssetURL(tag, name), resp.Status)
	}
}
