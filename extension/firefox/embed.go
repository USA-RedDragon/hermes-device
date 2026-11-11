// Package firefox embeds the Hermes Firefox extension's source, so tests
// and installers can write it out.
package firefox

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// Files holds the extension's files.
//
//go:embed manifest.json background.js pagechat.html pagechat.js pagechat.css icon.svg toolbar-light.svg toolbar-dark.svg
var Files embed.FS //nolint:gochecknoglobals // go:embed needs a variable

// ReleasesURL is where each release publishes the signed extension.
const ReleasesURL = "https://github.com/USA-RedDragon/talaria/releases"

// UpdateURL is the extension's update_url: the update manifest of the latest
// release.
const UpdateURL = ReleasesURL + "/latest/download/updates.json"

var (
	versionRE      = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})(\.(0|[1-9][0-9]{0,8})){0,3}$`)
	versionFieldRE = regexp.MustCompile(`(?m)^(\s*"version":\s*)"[^"]*"`)
)

// Write writes the extension's files to dir, with the manifest version set
// to version when that is not empty.
func Write(dir, version string) error {
	manifest, err := Manifest(version)
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
		data := manifest
		if p != "manifest.json" {
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
