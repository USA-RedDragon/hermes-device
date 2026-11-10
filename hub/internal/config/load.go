package config

import (
	"os"

	"github.com/USA-RedDragon/configulator/v2"
	cpflag "github.com/USA-RedDragon/configulator/v2/flags/pflag"
	"github.com/goccy/go-yaml"
	"github.com/spf13/pflag"
)

// EnvPrefix starts every environment variable name, as in HUB_HTTP_PORT.
const EnvPrefix = "HUB_"

// EnvSeparator joins nested levels in environment variable names.
const EnvSeparator = "_"

// ConfigPathEnv names the environment variable that sets the config file
// path. The --config flag takes precedence over it.
const ConfigPathEnv = "CONFIG"

// NewLoader returns a configulator for Config that reads config.yaml (or
// the file named by --config or CONFIG), environment variables and the
// flags it registers on fs.
func NewLoader(fs *pflag.FlagSet) *configulator.Configulator[Config] {
	c := configulator.New(ConfigSchema()).
		WithEnvironmentVariables(&configulator.EnvironmentVariableOptions{Prefix: EnvPrefix, Separator: EnvSeparator}).
		WithFile(&configulator.FileOptions{
			Search:   []string{"config.yaml"},
			Explicit: os.Getenv(ConfigPathEnv),
			Decoders: configulator.Decoders{".yaml": yaml.Unmarshal, ".yml": yaml.Unmarshal},
		})
	return cpflag.Bind(c, fs, ConfigPFlagHooks(), nil)
}
