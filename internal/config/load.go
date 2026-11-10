package config

import (
	"github.com/USA-RedDragon/configulator/v2"
	cpflag "github.com/USA-RedDragon/configulator/v2/flags/pflag"
	"github.com/goccy/go-yaml"
	"github.com/spf13/pflag"
)

// EnvPrefix starts every environment variable hermes-device reads, as in
// HERMES_DEVICE_HUB_URL.
const EnvPrefix = "HERMES_DEVICE_"

// EnvSeparator joins nested levels in environment variable names.
const EnvSeparator = "_"

// NewLoader returns a configulator for Config that reads the first file of
// search that exists (or the file named by --config), environment
// variables and the flags it registers on fs.
func NewLoader(fs *pflag.FlagSet, search []string) *configulator.Configulator[Config] {
	c := configulator.New(ConfigSchema()).
		WithEnvironmentVariables(&configulator.EnvironmentVariableOptions{Prefix: EnvPrefix, Separator: EnvSeparator}).
		WithFile(&configulator.FileOptions{
			Search:   search,
			Decoders: configulator.Decoders{".yaml": yaml.Unmarshal, ".yml": yaml.Unmarshal},
		})
	return cpflag.Bind(c, fs, ConfigPFlagHooks(), nil)
}
