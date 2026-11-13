package setup

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// DefaultReleasesURL is the repository's releases page.
const DefaultReleasesURL = "https://github.com/USA-RedDragon/talaria/releases"

// ReleasesURLEnv names an environment variable that replaces
// DefaultReleasesURL for setup and update. It is for testing a release
// that is not on GitHub: such a release has no attestation, so update also
// needs --insecure-skip-attestation.
const ReleasesURLEnv = "TALARIA_TEST_RELEASES_URL"

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
// <BaseURL>/latest. It uses no API, so no rate limit applies. A redirect to
// the latest release of a renamed repository is followed, up to a few.
func (r *Releases) LatestTag(ctx context.Context) (string, error) {
	c := *r.Client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	u := r.BaseURL + "/latest"
	for range 4 {
		resp, err := r.get(ctx, u, &c)
		if err != nil {
			return "", err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		if resp.StatusCode < 300 || resp.StatusCode > 399 {
			return "", fmt.Errorf("%s answered %s, not a redirect to the latest release", u, resp.Status)
		}
		loc, err := resp.Location()
		if err != nil {
			return "", err
		}
		dir, tag := path.Split(loc.Path)
		if strings.HasSuffix(dir, "/releases/tag/") && Canonical(tag) != "" {
			return tag, nil
		}
		if !strings.HasSuffix(loc.Path, "/releases/latest") {
			break
		}
		u = loc.String()
	}
	return "", fmt.Errorf("the latest release of %s redirects to no release tag", r.BaseURL)
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
	ext := "tar.gz"
	if goos == goosWindows {
		ext = "zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s.%s", BinaryName, version, goos, arch, ext)
}

// LegacyArchiveName is the archive name of releases from before the
// rename, which hold a hermes-device binary. Releases keep publishing it
// for the auto-update of older installs.
func LegacyArchiveName(version, goos, goarch, goarm string) string {
	return LegacyBinaryName + strings.TrimPrefix(ArchiveName(version, goos, goarch, goarm), BinaryName)
}

// ChecksumsName is the release's checksum file of version (without v).
func ChecksumsName(version string) string {
	return fmt.Sprintf("%s_%s_checksums.txt", BinaryName, version)
}

// LegacyChecksumsName is the checksum file name of releases up to v0.25.2.
// Releases keep publishing it for the auto-update of older installs.
func LegacyChecksumsName(version string) string {
	return fmt.Sprintf("%s_%s_checksums.txt", LegacyReleaseName, version)
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

// ExtractBinary returns the talaria file at the top of a tar.gz release
// archive, or the hermes-device file of a release from before the rename,
// or talaria.exe from a zip archive of a Windows release.
func ExtractBinary(archive []byte) ([]byte, error) {
	if bytes.HasPrefix(archive, []byte("PK\x03\x04")) {
		return extractZip(archive)
	}
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
		if name := path.Clean(h.Name); h.Typeflag == tar.TypeReg && (name == BinaryName || name == LegacyBinaryName) {
			if h.Size > maxArchive {
				return nil, fmt.Errorf("%s in the archive is too large", BinaryName)
			}
			return io.ReadAll(io.LimitReader(tr, maxArchive))
		}
	}
}

func extractZip(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	want := ExeName(goosWindows)
	for _, f := range zr.File {
		if path.Clean(f.Name) != want || !f.Mode().IsRegular() {
			continue
		}
		if f.UncompressedSize64 > maxArchive {
			return nil, fmt.Errorf("%s in the archive is too large", want)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, maxArchive))
	}
	return nil, fmt.Errorf("the archive has no %s", want)
}

// FetchArchive downloads the release archive of tag for an OS and
// architecture, checks it against the release's checksum file and returns
// it with its name and SHA-256 digest. A release from before the rename has
// only the legacy archive.
func (r *Releases) FetchArchive(ctx context.Context, tag, goos, goarch, goarm string) ([]byte, string, []byte, error) {
	v := strings.TrimPrefix(tag, "v")
	sums, err := r.Fetch(ctx, tag, ChecksumsName(v), maxChecksums)
	if errors.Is(err, ErrNoAsset) {
		sums, err = r.Fetch(ctx, tag, LegacyChecksumsName(v), maxChecksums)
	}
	if err != nil {
		return nil, "", nil, err
	}
	listed := ParseChecksums(sums)
	name := ArchiveName(v, goos, goarch, goarm)
	want, ok := listed[name]
	if !ok {
		name = LegacyArchiveName(v, goos, goarch, goarm)
		want, ok = listed[name]
	}
	if !ok {
		return nil, "", nil, fmt.Errorf("%w: %s lists no %s (no build for %s/%s?)", ErrNoAsset, ChecksumsName(v),
			ArchiveName(v, goos, goarch, goarm), goos, goarch)
	}
	archive, err := r.Fetch(ctx, tag, name, maxArchive)
	if err != nil {
		return nil, "", nil, err
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, "", nil, fmt.Errorf("%w: %s has sha256 %s, the checksum file says %s", ErrChecksum, name, got, want)
	}
	return archive, name, sum[:], nil
}

// XPIName is the signed Firefox extension file of version (without v).
func XPIName(version string) string { return "talaria-firefox-" + version + ".xpi" }

// updatesLimit caps the size of a release's updates.json.
const updatesLimit = 64 << 10

// XPIURL returns the download URL of the signed Firefox extension of release
// tag. A release whose extension did not change carries the earlier signed
// file, named for the version it was signed as, and its updates.json links
// to it.
func (r *Releases) XPIURL(ctx context.Context, tag string) (string, error) {
	name := XPIName(strings.TrimPrefix(tag, "v"))
	ok, err := r.Has(ctx, tag, name)
	if err != nil {
		return "", err
	}
	if ok {
		return r.AssetURL(tag, name), nil
	}
	data, err := r.Fetch(ctx, tag, "updates.json", updatesLimit)
	if err != nil {
		return "", err
	}
	var u struct {
		Addons map[string]struct {
			Updates []struct {
				Link string `json:"update_link"`
			} `json:"updates"`
		} `json:"addons"`
	}
	if err := json.Unmarshal(data, &u); err != nil {
		return "", fmt.Errorf("%s of %s: %w", "updates.json", tag, err)
	}
	for _, a := range u.Addons {
		for _, up := range a.Updates {
			if strings.HasPrefix(up.Link, r.BaseURL+"/download/") && strings.HasSuffix(up.Link, ".xpi") {
				return up.Link, nil
			}
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNoAsset, r.AssetURL(tag, name))
}

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
