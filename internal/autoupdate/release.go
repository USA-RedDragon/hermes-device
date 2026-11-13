// Package autoupdate finds new talaria releases and installs them
// from the run daemon, and tells a person who runs a command about them.
package autoupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/internal/legacy"
	"golang.org/x/mod/semver"
)

// DefaultAPIURL is the GitHub API.
const DefaultAPIURL = "https://api.github.com"

// Limits of the release lookup.
const (
	maxBody = 1 << 20
	// maxNotBefore caps a rate-limit pause read from GitHub or the cache.
	maxNotBefore = 24 * time.Hour
	// defaultPause is the pause after a rate limit answer without a time.
	defaultPause = time.Hour
)

// ErrRateLimited is returned while GitHub's rate limit pauses lookups.
var ErrRateLimited = errors.New("GitHub rate limit: no release lookup until later")

// Stable reports whether tag is a release tag vX.Y.Z without a pre-release
// or build part.
func Stable(tag string) bool {
	return strings.HasPrefix(tag, "v") && semver.IsValid(tag) && semver.Canonical(tag) == tag && semver.Prerelease(tag) == ""
}

// Newer reports whether stable release tag is newer than version (with or
// without v). A version that is not a stable release, such as a
// development build, has nothing newer.
func Newer(tag, version string) bool {
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	return Stable(tag) && Stable(version) && semver.Compare(tag, version) > 0
}

// entry is the cache file. It is a hint only: a tag from it is checked
// again before use, and an install verifies the release from scratch.
type entry struct {
	ETag      string    `json:"etag,omitempty"`
	Tag       string    `json:"tag,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	TriedAt   time.Time `json:"tried_at"`
	NotBefore time.Time `json:"not_before"`
}

// Releases looks up the latest release on the GitHub API and caches the
// answer with its ETag in a file that the daemon and the CLI share. Every
// lookup counts against GitHub's limit of 60 unauthenticated requests per
// hour per IP address, also one answered 304, so callers reuse a recent
// answer: the CLI a day old, the daemon an hour old.
type Releases struct {
	// APIURL is DefaultAPIURL, or a test server.
	APIURL      string
	Owner, Repo string
	Client      *http.Client
	UserAgent   string
	// Path is the cache file; empty means no cache.
	Path string
	// Now is time.Now unless a test sets it.
	Now func() time.Time
}

// DefaultCachePath is release.json in talaria's user cache
// directory, or "" when there is none.
func DefaultCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(legacy.Dir(filepath.Join(dir, legacy.Name)), "release.json")
}

func (r *Releases) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// load reads the cache and drops what cannot be right: a tag that is not
// a stable release or a bad ETag (then all but the pause), a check in the
// future, and a pause longer than maxNotBefore.
func (r *Releases) load() entry {
	var e entry
	if r.Path == "" {
		return e
	}
	data, err := os.ReadFile(r.Path)
	if err != nil || len(data) > maxBody || json.Unmarshal(data, &e) != nil {
		return entry{}
	}
	now := r.now()
	if e.Tag != "" && !Stable(e.Tag) || !validETag(e.ETag) {
		return entry{NotBefore: e.NotBefore}
	}
	if e.CheckedAt.After(now) {
		e.CheckedAt = time.Time{}
	}
	if e.TriedAt.After(now) {
		e.TriedAt = time.Time{}
	}
	if e.NotBefore.After(now.Add(maxNotBefore)) {
		e.NotBefore = time.Time{}
	}
	return e
}

// validETag reports whether an ETag is safe to send back: printable ASCII,
// not too long.
func validETag(s string) bool {
	if len(s) > 256 {
		return false
	}
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// save writes the cache atomically. A failure only costs a request later.
func (r *Releases) save(e entry) {
	if r.Path == "" {
		return
	}
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	dir := filepath.Dir(r.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".release-*.json")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), r.Path)
}

// Cached returns the latest release tag in the cache, "" when it has none,
// and when it was checked.
func (r *Releases) Cached() (string, time.Time) {
	e := r.load()
	return e.Tag, e.CheckedAt
}

// Latest returns the tag of the latest stable release. It answers from
// the cache, without a request, when someone tried a lookup less than
// maxAge ago (the tag is then "" when that lookup failed) and while
// GitHub's rate limit pauses lookups (then with ErrRateLimited). Otherwise
// it asks GitHub, with the cached ETag.
func (r *Releases) Latest(ctx context.Context, maxAge time.Duration) (string, error) {
	e := r.load()
	now := r.now()
	if !e.TriedAt.IsZero() && now.Sub(e.TriedAt) < maxAge {
		return e.Tag, nil
	}
	if now.Before(e.NotBefore) {
		return "", fmt.Errorf("%w (until %s)", ErrRateLimited, e.NotBefore.Format(time.RFC3339))
	}
	e.TriedAt = now
	r.save(e)
	base := r.APIURL
	if base == "" {
		base = DefaultAPIURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(base, "/")+"/repos/"+r.Owner+"/"+r.Repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if r.UserAgent != "" {
		req.Header.Set("User-Agent", r.UserAgent)
	}
	if e.Tag != "" && e.ETag != "" {
		req.Header.Set("If-None-Match", e.ETag)
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("look up the latest release: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return "", fmt.Errorf("look up the latest release: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotModified && e.Tag != "":
		e.CheckedAt, e.NotBefore = now, time.Time{}
		r.save(e)
		return e.Tag, nil
	case resp.StatusCode == http.StatusOK:
		if len(body) > maxBody {
			return "", errors.New("look up the latest release: the answer is too large")
		}
		var rel struct {
			TagName    string `json:"tag_name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
		}
		if err := json.Unmarshal(body, &rel); err != nil {
			return "", fmt.Errorf("look up the latest release: %w", err)
		}
		if rel.Draft || rel.Prerelease || !Stable(rel.TagName) {
			return "", fmt.Errorf("the latest release %q is not a stable release", rel.TagName)
		}
		etag := resp.Header.Get("ETag")
		if !validETag(etag) {
			etag = ""
		}
		r.save(entry{ETag: etag, Tag: rel.TagName, CheckedAt: now, TriedAt: now})
		return rel.TagName, nil
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		e.NotBefore = now.Add(pause(resp.Header, now))
		r.save(e)
		return "", fmt.Errorf("%w (GitHub answered %s; until %s)", ErrRateLimited, resp.Status, e.NotBefore.Format(time.RFC3339))
	default:
		return "", fmt.Errorf("look up the latest release: GitHub answered %s", resp.Status)
	}
}

// pause is how long a rate limit answer asks to wait: Retry-After, else
// until X-RateLimit-Reset, else defaultPause; at least a minute and at
// most maxNotBefore.
func pause(h http.Header, now time.Time) time.Duration {
	d := defaultPause
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil {
		d = time.Duration(s) * time.Second
	} else if s, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		d = time.Unix(s, 0).Sub(now)
	}
	return min(max(d, time.Minute), maxNotBefore)
}
