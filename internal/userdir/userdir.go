// Package userdir finds the user directory that talaria keeps its config,
// token and state in.
package userdir

import (
	"os"
	"runtime"
)

// Config is the base of talaria's config directory: the user config
// directory (~/.config, ~/Library/Application Support), or on Windows
// %LOCALAPPDATA%, so that the config and the token stay on this computer
// instead of roaming with the profile. Callers add the talaria folder.
func Config() (string, error) {
	if runtime.GOOS == "windows" {
		return os.UserCacheDir()
	}
	return os.UserConfigDir()
}
