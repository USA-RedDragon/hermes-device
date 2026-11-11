package setup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ExtensionID is the Hermes Firefox extension's id.
const ExtensionID = "browser@hermes-device"

// Firefox is what setup found of Firefox.
type Firefox struct {
	// Command opens a URL in Firefox: the program and the arguments
	// before the URL. Empty when setup cannot start Firefox.
	Command []string
	// Profiles are the profile directories setup found.
	Profiles []string
	// Flatpak is true when a profile is in the Flatpak sandbox, where the
	// native messaging host does not reach the daemon.
	Flatpak bool
}

// Found reports whether Firefox looks installed.
func (f *Firefox) Found() bool { return len(f.Command) > 0 || len(f.Profiles) > 0 }

func (s *System) profileRoots() []string {
	h := s.Home
	if s.GOOS == darwin {
		return []string{filepath.Join(h, "Library", "Application Support", "Firefox", "Profiles")}
	}
	return []string{
		filepath.Join(h, ".mozilla", "firefox"),
		filepath.Join(h, ".config", "mozilla", "firefox"),
		filepath.Join(h, "snap", "firefox", "common", ".mozilla", "firefox"),
		filepath.Join(h, ".var", "app", "org.mozilla.firefox", ".mozilla", "firefox"),
		filepath.Join(h, ".var", "app", "org.mozilla.firefox", "config", "mozilla", "firefox"),
	}
}

// FindFirefox looks for the Firefox program and its profiles.
func (s *System) FindFirefox() *Firefox {
	f := &Firefox{}
	if s.GOOS == darwin {
		for _, app := range []string{"/Applications/Firefox.app", filepath.Join(s.Home, "Applications", "Firefox.app")} {
			if fi, err := os.Stat(app); err == nil && fi.IsDir() {
				f.Command = []string{"open", "-a", app}
				break
			}
		}
	} else {
		for _, name := range []string{"firefox", "firefox-esr", "firefox-developer-edition", "firefox-nightly"} {
			if p, err := s.LookPath(name); err == nil {
				f.Command = []string{p}
				break
			}
		}
	}
	for _, root := range s.profileRoots() {
		matches, _ := filepath.Glob(filepath.Join(root, "*", "prefs.js"))
		for _, m := range matches {
			f.Profiles = append(f.Profiles, filepath.Dir(m))
			if strings.Contains(m, filepath.Join(".var", "app", "org.mozilla.firefox")) {
				f.Flatpak = true
			}
		}
	}
	if len(f.Command) == 0 && f.Flatpak {
		if p, err := s.LookPath("flatpak"); err == nil {
			f.Command = []string{p, "run", "org.mozilla.firefox"}
		}
	}
	return f
}

// ExtensionInstalled reports whether a profile has the Hermes extension
// installed and not disabled.
func (f *Firefox) ExtensionInstalled() bool {
	for _, p := range f.Profiles {
		data, err := os.ReadFile(filepath.Join(p, "extensions.json"))
		if err != nil {
			continue
		}
		var ext struct {
			Addons []struct {
				ID           string `json:"id"`
				UserDisabled bool   `json:"userDisabled"`
			} `json:"addons"`
		}
		if json.Unmarshal(data, &ext) != nil {
			continue
		}
		for _, a := range ext.Addons {
			if a.ID == ExtensionID && !a.UserDisabled {
				return true
			}
		}
	}
	return false
}

// Open opens url in Firefox.
func (s *System) Open(_ context.Context, f *Firefox, url string) error {
	args := append(append([]string{}, f.Command[1:]...), url)
	return s.Run.Start(f.Command[0], args...)
}
