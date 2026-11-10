package config_test

import (
	"errors"
	"testing"
	"time"

	"github.com/USA-RedDragon/configulator/v2"
	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/goccy/go-yaml"
)

func load(env map[string]string, file string) (*config.Config, error) {
	c := configulator.New(config.ConfigSchema()).
		WithEnvironmentVariables(&configulator.EnvironmentVariableOptions{Prefix: config.EnvPrefix, Separator: config.EnvSeparator}).
		WithFile(&configulator.FileOptions{Search: []string{"config.yaml"}, Decoders: configulator.Decoders{".yaml": yaml.Unmarshal}}).
		WithEnviron(func(k string) (string, bool) { v, ok := env[k]; return v, ok }).
		WithReadFile(func(string) ([]byte, error) { return []byte(file), nil })
	return c.Load()
}

func TestLoadFileAndEnv(t *testing.T) {
	t.Parallel()
	cfg, err := load(map[string]string{"HERMES_DEVICE_HUB_URL": "https://hub.example.com"}, `
device:
  roots: [/home/alice/src, /home/alice/work]
acp-host:
  backend-id: alice
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hub.URL != "https://hub.example.com" || len(cfg.Device.Roots) != 2 || cfg.Device.Shell != "bash" ||
		cfg.PingInterval != 25*time.Second || !cfg.Store.Keyring || cfg.ACPHost.BackendID != "alice" {
		t.Errorf("cfg = %+v", cfg)
	}
	if err := cfg.ForRun(); err != nil {
		t.Errorf("ForRun: %v", err)
	}
	if err := cfg.ForACPHost(); !errors.Is(err, config.ErrMissing) {
		t.Errorf("ForACPHost: %v", err)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		file string
		want error
	}{
		"bad hub url":   {"hub: {url: ws://x}\n", config.ErrInvalidHubURL},
		"relative root": {"device: {roots: [rel]}\n", config.ErrRelativePath},
		"public splice": {"gateway: {listen: \"0.0.0.0:1\"}\n", config.ErrNotLoopback},
		"slow pings":    {"ping-interval: 1m\n", config.ErrPingInterval},
		"no processes":  {"acp-host: {max-processes: 0}\n", config.ErrMaxProcesses},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := load(nil, tc.file); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	cfg, err := load(nil, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ForHub(); !errors.Is(err, config.ErrMissing) {
		t.Errorf("ForHub: %v", err)
	}
}
