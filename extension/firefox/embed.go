// Package firefox embeds the Hermes Firefox extension's source, so tests
// and installers can write it out.
package firefox

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// Files holds manifest.json and background.js.
//
//go:embed manifest.json background.js
var Files embed.FS //nolint:gochecknoglobals // go:embed needs a variable

// ReleasesURL is where each release publishes the signed extension.
const ReleasesURL = "https://github.com/USA-RedDragon/hermes-device/releases"

// UpdateURL is the extension's update_url: the update manifest of the latest
// release.
const UpdateURL = ReleasesURL + "/latest/download/updates.json"

var (
	versionRE      = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})(\.(0|[1-9][0-9]{0,8})){0,3}$`)
	versionFieldRE = regexp.MustCompile(`(?m)^(\s*"version":\s*)"[^"]*"`)
)

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
