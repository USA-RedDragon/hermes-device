package config

import (
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/USA-RedDragon/configulator/v2"
	cpflag "github.com/USA-RedDragon/configulator/v2/flags/pflag"
	"github.com/goccy/go-yaml"
	"github.com/spf13/pflag"
)

// EnvPrefix starts every environment variable talaria reads, as in
// TALARIA_HUB_URL.
const EnvPrefix = "TALARIA_"

// LegacyEnvPrefix is the prefix of releases before the rename to talaria.
// A variable with it is still read when the TALARIA_ one is unset, with a
// deprecation warning.
const LegacyEnvPrefix = "HERMES_DEVICE_"

// EnvSeparator joins nested levels in environment variable names.
const EnvSeparator = "_"

//nolint:gochecknoglobals // warn once per variable and process
var warned sync.Map

// LookupEnv looks up the TALARIA_ variable name, and when it is unset, the
// same variable with the legacy prefix, which it warns about once.
func LookupEnv(name string) (string, bool) {
	return lookupEnv(os.LookupEnv, name)
}

// Getenv is LookupEnv without the found flag.
func Getenv(name string) string {
	v, _ := LookupEnv(name)
	return v
}

// LegacyEnviron wraps lookup so that a TALARIA_ variable that is unset
// falls back to the legacy name, as LookupEnv does.
func LegacyEnviron(lookup func(string) (string, bool)) func(string) (string, bool) {
	return func(name string) (string, bool) { return lookupEnv(lookup, name) }
}

func lookupEnv(lookup func(string) (string, bool), name string) (string, bool) {
	if v, ok := lookup(name); ok {
		return v, true
	}
	rest, ok := strings.CutPrefix(name, EnvPrefix)
	if !ok {
		return "", false
	}
	legacy := LegacyEnvPrefix + rest
	v, ok := lookup(legacy)
	if ok {
		if _, done := warned.LoadOrStore(legacy, true); !done {
			slog.Warn("The environment variable "+legacy+" is deprecated; use "+name+". A later release stops reading it.", "variable", legacy)
		}
	}
	return v, ok
}

// NewLoader returns a configulator for Config that reads the first file of
// search that exists (or the file named by --config), environment
// variables and the flags it registers on fs.
func NewLoader(fs *pflag.FlagSet, search []string) *configulator.Configulator[Config] {
	c := configulator.New(ConfigSchema()).
		WithEnvironmentVariables(&configulator.EnvironmentVariableOptions{Prefix: EnvPrefix, Separator: EnvSeparator}).
		WithEnviron(LookupEnv).
		WithFile(&configulator.FileOptions{
			Search:   search,
			Decoders: configulator.Decoders{".yaml": yaml.Unmarshal, ".yml": yaml.Unmarshal},
		})
	return cpflag.Bind(c, fs, ConfigPFlagHooks(), nil)
}
