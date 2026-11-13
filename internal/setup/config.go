package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"

	"github.com/goccy/go-yaml"
)

// ErrHubURL is returned for a hub URL that is not http or https with a
// host.
var ErrHubURL = errors.New("the hub URL must be an http:// or https:// URL with a host")

// CheckHubURL checks a hub URL as config.Validate does.
func CheckHubURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: %q", ErrHubURL, raw)
	}
	return nil
}

// FileConfig is the part of the config file that setup writes.
type FileConfig struct {
	Hub struct {
		URL string `yaml:"url"`
	} `yaml:"hub"`
	Device struct {
		Roots     []string `yaml:"roots"`
		Shell     string   `yaml:"shell,omitempty"`
		WSLDistro string   `yaml:"wsl-distro,omitempty"`
	} `yaml:"device"`
	Browser struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"browser"`
}

const configHeader = "# Written by talaria setup. config.example.yaml in the repository lists every setting.\n"

// ConfigYAML is the config file setup writes. shell and distro are the
// Windows shell (internal/winshell) and its WSL distribution, or empty.
func ConfigYAML(hub string, roots []string, browser bool, shell, distro string) ([]byte, error) {
	var c FileConfig
	c.Hub.URL = hub
	c.Device.Roots = roots
	c.Device.Shell = shell
	if shell == shellWSL {
		c.Device.WSLDistro = distro
	}
	c.Browser.Enabled = browser
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	return append([]byte(configHeader), data...), nil
}

// WriteConfig writes data to path when path does not exist, or when
// overwrite is set, after a backup of the old file. It returns the backup
// path and whether it wrote.
func (s *System) WriteConfig(path string, data []byte, overwrite bool) (backup string, wrote bool, err error) {
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", false, err
	case string(old) == string(data):
		return "", false, nil
	case !overwrite:
		return "", false, nil
	default:
		if backup, err = s.backup(path); err != nil {
			return "", false, err
		}
	}
	return backup, true, writeFileAtomic(path, data, 0o600)
}
