// Package firefox embeds the Hermes Firefox extension's source, so tests
// and installers can write it out.
package firefox

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Files holds the extension's files.
//
//go:embed manifest.json background.js defaults.js hub.js popup.html popup.js popup.css options.html options.js
var Files embed.FS //nolint:gochecknoglobals // go:embed needs a variable

// ReleasesURL is where each release publishes the signed extension.
const ReleasesURL = "https://github.com/USA-RedDragon/hermes-device/releases"

// UpdateURL is the extension's update_url: the update manifest of the latest
// release.
const UpdateURL = ReleasesURL + "/latest/download/updates.json"

var (
	versionRE      = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})(\.(0|[1-9][0-9]{0,8})){0,3}$`)
	versionFieldRE = regexp.MustCompile(`(?m)^(\s*"version":\s*)"[^"]*"`)
	defaultHubRE   = regexp.MustCompile(`(?m)^const DEFAULT_HUB_URL = "";$`)
)

// ErrHubURL is returned for a default hub address the extension would not
// accept.
var ErrHubURL = errors.New("the hub address must be an https URL (http only for localhost) without a user, password, query or fragment")

// ValidHubURL checks a hub address as the extension's normalizeHubURL does
// and returns it without a trailing slash.
func ValidHubURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", fmt.Errorf("%w: %q", ErrHubURL, raw)
	}
	host := u.Hostname()
	local := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if (u.Scheme != "https" && (u.Scheme != "http" || !local)) || u.User != nil || strings.ContainsAny(raw, "?#") {
		return "", fmt.Errorf("%w: %q", ErrHubURL, raw)
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// Defaults returns defaults.js with DEFAULT_HUB_URL set to hubURL, or
// unchanged when hubURL is empty.
func Defaults(hubURL string) ([]byte, error) {
	data, err := Files.ReadFile("defaults.js")
	if err != nil || hubURL == "" {
		return data, err
	}
	hub, err := ValidHubURL(hubURL)
	if err != nil {
		return nil, err
	}
	val, err := json.Marshal(hub)
	if err != nil {
		return nil, err
	}
	if !defaultHubRE.Match(data) {
		return nil, errors.New("defaults.js has no DEFAULT_HUB_URL line")
	}
	return defaultHubRE.ReplaceAllLiteral(data, []byte("const DEFAULT_HUB_URL = "+string(val)+";")), nil
}

// Write writes the extension's files to dir, with the manifest version set
// to version and the default hub address set to hubURL, when those are not
// empty.
func Write(dir, version, hubURL string) error {
	manifest, err := Manifest(version)
	if err != nil {
		return err
	}
	defaults, err := Defaults(hubURL)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // extension source, no secret
		return err
	}
	return fs.WalkDir(Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		var data []byte
		switch p {
		case "manifest.json":
			data = manifest
		case "defaults.js":
			data = defaults
		default:
			if data, err = Files.ReadFile(p); err != nil {
				return err
			}
		}
		return os.WriteFile(filepath.Join(dir, p), data, 0o644) //nolint:gosec // as above
	})
}

// ErrVersion is returned for a version Firefox and addons.mozilla.org do not
// accept.
var ErrVersion = errors.New("an extension version is one to four dot-separated numbers of up to nine digits without leading zeros")

// ValidVersion reports whether v is a version that addons.mozilla.org signs.
func ValidVersion(v string) bool {
	return versionRE.MatchString(v)
}

// Manifest returns manifest.json with its version set to version, or
// unchanged when version is empty. Only the version line changes.
func Manifest(version string) ([]byte, error) {
	data, err := Files.ReadFile("manifest.json")
	if err != nil || version == "" {
		return data, err
	}
	if !ValidVersion(version) {
		return nil, fmt.Errorf("%w: %q", ErrVersion, version)
	}
	loc := versionFieldRE.FindSubmatchIndex(data)
	if loc == nil {
		return nil, errors.New("manifest.json has no version line")
	}
	out := make([]byte, 0, len(data)+len(version))
	out = append(out, data[:loc[3]]...)
	out = append(out, '"')
	out = append(out, version...)
	out = append(out, '"')
	out = append(out, data[loc[1]:]...)
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("manifest.json after setting the version: %w", err)
	}
	if m.Version != version {
		return nil, fmt.Errorf("manifest.json has version %q after setting it to %q", m.Version, version)
	}
	return out, nil
}
