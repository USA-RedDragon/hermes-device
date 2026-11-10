package config_test

import (
	"errors"
	"testing"

	"github.com/USA-RedDragon/configulator/v2"
	"github.com/USA-RedDragon/hermes-device/client/internal/config"
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
	cfg, err := load(map[string]string{"HERMES_DEVICE_DEVICE_TOKEN": "tok"}, `
server:
  url: ws://127.0.0.1:9119/api/plugins/device/ws
device:
  name: desktop
roots: [/home/me/src, /home/me/work]
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Device.Token != "tok" || cfg.Device.Name != "desktop" || len(cfg.Roots) != 2 || cfg.Shell != "bash" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestRequiredAndValidate(t *testing.T) {
	t.Parallel()
	var req *configulator.RequiredError
	if _, err := load(nil, "{}"); !errors.As(err, &req) {
		t.Errorf("empty config: %v", err)
	}
	base := "device: {name: a, token: b}\nroots: [/x]\n"
	if _, err := load(nil, base+"server: {url: http://x}\n"); !errors.Is(err, config.ErrInvalidURL) {
		t.Errorf("http url: %v", err)
	}
	if _, err := load(nil, "server: {url: ws://x}\ndevice: {name: a, token: b}\nroots: [rel]\n"); !errors.Is(err, config.ErrRelativePath) {
		t.Errorf("relative root: %v", err)
	}
}
