// Package plugin embeds the Hermes device plugin's files so the
// talaria binary can install them (talaria plugin install).
package plugin

import (
	"embed"
	"io/fs"
)

//go:embed device/*.py device/plugin.yaml device/dashboard/*.py device/dashboard/manifest.json
var files embed.FS

// Files returns the plugin tree rooted at the plugin directory, so paths
// look like __init__.py and dashboard/plugin_api.py. Tests are not part of
// it.
func Files() fs.FS {
	sub, err := fs.Sub(files, "device")
	if err != nil {
		panic(err)
	}
	return sub
}
