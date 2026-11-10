// Package firefox embeds the Hermes Firefox extension's source, so tests
// and installers can write it out.
package firefox

import "embed"

// Files holds manifest.json and background.js.
//
//go:embed manifest.json background.js
var Files embed.FS //nolint:gochecknoglobals // go:embed needs a variable
