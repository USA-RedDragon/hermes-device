package claudeusage

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

// DefaultSources are the Claude Code directories of this computer: each
// directory in $CLAUDE_CONFIG_DIR (comma separated, as Claude Code and
// ccusage read it), else ~/.claude with ~/.claude.json, then extra.
func DefaultSources(home string, getenv func(string) string, extra []string) []Source {
	var out []Source
	seen := map[string]bool{}
	add := func(s Source) {
		s.Dir = filepath.Clean(s.Dir)
		if seen[s.Dir] {
			return
		}
		seen[s.Dir] = true
		out = append(out, s)
	}
	if env := getenv("CLAUDE_CONFIG_DIR"); env != "" {
		for _, d := range strings.Split(env, ",") {
			if d = strings.TrimSpace(d); filepath.IsAbs(d) {
				add(Source{Dir: d})
			}
		}
	} else if home != "" {
		add(Source{Dir: filepath.Join(home, ".claude"), ConfigFile: homeConfig(home)})
	}
	for _, d := range extra {
		add(Source{Dir: d})
	}
	return out
}

// homeConfig is the .claude.json of the default config directory:
// ~/.claude.json, or the older ~/.claude/.claude.json.
func homeConfig(home string) string {
	p := filepath.Join(home, ".claude.json")
	if _, err := os.Stat(p); err != nil {
		if q := filepath.Join(home, ".claude", ".claude.json"); fileExists(q) {
			return q
		}
	}
	return p
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

var slotID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Slot is one account slot of the claude-accounts Hermes plugin.
type Slot struct {
	ID    string `yaml:"id"`
	Label string `yaml:"label"`
}

// HermesSlots reads the slots in the claude-accounts plugin's settings
// (plugins.entries.claude-accounts.settings.accounts) of a Hermes
// config.yaml. A bad or repeated id is skipped, as the plugin does.
func HermesSlots(configYAML string) []Slot {
	data, err := os.ReadFile(configYAML)
	if err != nil {
		return nil
	}
	var cfg struct {
		Plugins struct {
			Entries map[string]struct {
				Settings struct {
					Accounts []Slot `yaml:"accounts"`
				} `yaml:"settings"`
			} `yaml:"entries"`
		} `yaml:"plugins"`
	}
	if yaml.Unmarshal(data, &cfg) != nil {
		return nil
	}
	var out []Slot
	seen := map[string]bool{}
	for _, s := range cfg.Plugins.Entries["claude-accounts"].Settings.Accounts {
		if !slotID.MatchString(s.ID) || seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		if strings.TrimSpace(s.Label) == "" {
			s.Label = s.ID
		}
		out = append(out, s)
	}
	return out
}

// HermesSources are the directories of the slots in root, labeled by
// slots, then each other slot directory in root under its own id.
func HermesSources(root string, slots []Slot) []Source {
	var out []Source
	seen := map[string]bool{}
	for _, s := range slots {
		seen[s.ID] = true
		out = append(out, Source{Dir: filepath.Join(root, s.ID), Label: s.Label, Slot: s.ID, OwnAccount: true})
	}
	ents, _ := os.ReadDir(root)
	for _, e := range ents {
		if e.IsDir() && slotID.MatchString(e.Name()) && !seen[e.Name()] {
			out = append(out, Source{Dir: filepath.Join(root, e.Name()), Label: e.Name(), Slot: e.Name(), OwnAccount: true})
		}
	}
	return out
}

// InContainer reports whether this process runs in a container: in a
// Kubernetes pod, or with Docker's or Podman's marker file.
func InContainer(getenv func(string) string) bool {
	return getenv("KUBERNETES_SERVICE_HOST") != "" || fileExists("/.dockerenv") || fileExists("/run/.containerenv")
}
