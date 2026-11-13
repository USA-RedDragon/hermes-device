package config_test

import (
	"errors"
	"testing"
	"time"

	"github.com/USA-RedDragon/configulator/v2"
	"github.com/USA-RedDragon/talaria/internal/config"
	"github.com/USA-RedDragon/talaria/internal/setup"
	"github.com/goccy/go-yaml"
)

const (
	hubURL    = "https://hub.example.com"
	hubURLEnv = "TALARIA_HUB_URL"
	aliceHome = "/home/alice"
	workRoot  = "/work"
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
	cfg, err := load(map[string]string{"TALARIA_HUB_URL": hubURL}, `
device:
  roots: [/home/alice/src, /home/alice/work]
acp-host:
  backend-id: alice
`)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hub.URL != hubURL || len(cfg.Device.Roots) != 2 || cfg.Device.Shell != "bash" ||
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
		"bad hub url":          {"hub: {url: ws://x}\n", config.ErrInvalidHubURL},
		"relative root":        {"device: {roots: [rel]}\n", config.ErrRelativePath},
		"relative claude dir":  {"usage: {claude-dirs: [rel]}\n", config.ErrRelativePath},
		"relative usage state": {"usage: {state-file: rel}\n", config.ErrRelativePath},
		"public splice":        {"gateway: {listen: \"0.0.0.0:1\"}\n", config.ErrNotLoopback},
		"slow pings":           {"ping-interval: 1m\n", config.ErrPingInterval},
		"no processes":         {"acp-host: {max-processes: 0}\n", config.ErrMaxProcesses},
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
	if !cfg.Gateway.Splice {
		t.Error("gateway.splice defaults to false")
	}
}

func TestServiceDeviceFromEnv(t *testing.T) {
	t.Parallel()
	cfg, err := load(map[string]string{ //nolint:gosec // a fake token
		"TALARIA_HUB_URL": hubURL, "TALARIA_HUB_TOKEN_FILE": "/var/run/secrets/token",
		"TALARIA_HUB_TOKEN": "hdt_x", "TALARIA_DEVICE_ROOTS": "/work", "TALARIA_GATEWAY_SPLICE": "false",
		"TALARIA_STORE_KEYRING": "false",
	}, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hub.TokenFile != "/var/run/secrets/token" || cfg.Hub.Token != "hdt_x" || cfg.Gateway.Splice ||
		cfg.Store.Keyring || len(cfg.Device.Roots) != 1 {
		t.Errorf("cfg = %+v", cfg)
	}
	if err := cfg.ForRun(); err != nil {
		t.Errorf("ForRun: %v", err)
	}
	if _, err := load(nil, "hub: {token-file: rel}\n"); !errors.Is(err, config.ErrRelativePath) {
		t.Errorf("relative token file: %v", err)
	}
}

func TestMCPServers(t *testing.T) {
	t.Parallel()
	cfg, err := load(nil, `
mcp:
  servers:
    rea:
      command: npx
      args: ["-y", "rea-agents@6.7.0", "mcp"]
      env:
        HOPPER_LAUNCHER_PATH: /opt/hopper/bin/Hopper
      idle-timeout: 12h
    tiny:
      command: /usr/local/bin/tiny-mcp
      dir: /home/alice
`)
	if err != nil {
		t.Fatal(err)
	}
	rea := cfg.MCP.Servers["rea"]
	if len(cfg.MCP.Servers) != 2 || rea.Command != "npx" || len(rea.Args) != 3 || rea.Args[1] != "rea-agents@6.7.0" ||
		rea.Env["HOPPER_LAUNCHER_PATH"] != "/opt/hopper/bin/Hopper" || cfg.MCP.Servers["tiny"].Dir != "/home/alice" ||
		cfg.MCP.IdleTimeout != time.Hour {
		t.Errorf("mcp = %+v", cfg.MCP)
	}
	if rea.IdleTimeout == nil || *rea.IdleTimeout != 12*time.Hour || cfg.MCP.Servers["tiny"].IdleTimeout != nil {
		t.Errorf("per-server idle-timeout: rea %v, tiny %v", rea.IdleTimeout, cfg.MCP.Servers["tiny"].IdleTimeout)
	}
	for name, file := range map[string]string{
		"name":        "mcp: {servers: {Bad.Name: {command: x}}}\n",
		"command":     "mcp: {servers: {rea: {args: [x]}}}\n",
		"dir":         "mcp: {servers: {rea: {command: x, dir: rel}}}\n",
		"env name":    "mcp: {servers: {rea: {command: x, env: {\"A=B\": c}}}}\n",
		"idle":        "mcp: {idle-timeout: -1s}\n",
		"server idle": "mcp: {servers: {rea: {command: x, idle-timeout: -1s}}}\n",
	} {
		if _, err := load(nil, file); !errors.Is(err, config.ErrMCPServer) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRootsDefaultToHome(t *testing.T) {
	t.Parallel()
	cfg, err := load(map[string]string{hubURLEnv: hubURL}, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ForRun(); err != nil {
		t.Errorf("ForRun without roots: %v", err)
	}
	home := func(h string, err error) func() (string, error) { return func() (string, error) { return h, err } }
	if roots, err := cfg.Roots(home("/home/alice/", nil)); err != nil || len(roots) != 1 || roots[0] != aliceHome {
		t.Errorf("Roots = %q, %v", roots, err)
	}
	for _, h := range []string{"/", "", "rel"} {
		if _, err := cfg.Roots(home(h, nil)); !errors.Is(err, config.ErrNoHomeRoot) {
			t.Errorf("home %q: %v", h, err)
		}
	}
	if _, err := cfg.Roots(home("", errors.New("no home"))); !errors.Is(err, config.ErrNoHomeRoot) {
		t.Errorf("home error: %v", err)
	}
	cfg.Device.Roots = []string{workRoot}
	if roots, err := cfg.Roots(home("/", nil)); err != nil || len(roots) != 1 || roots[0] != workRoot {
		t.Errorf("set roots: %q, %v", roots, err)
	}
}

func TestSetupConfigLoads(t *testing.T) {
	t.Parallel()
	data, err := setup.ConfigYAML(hubURL, []string{aliceHome}, true)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := load(nil, string(data))
	if err != nil {
		t.Fatalf("load:\n%s\n%v", data, err)
	}
	if cfg.Hub.URL != hubURL || len(cfg.Device.Roots) != 1 || cfg.Device.Roots[0] != aliceHome || !cfg.Browser.Enabled {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLegacyEnvironmentVariables(t *testing.T) {
	t.Parallel()
	env := map[string]string{"HERMES_DEVICE_HUB_URL": hubURL, "HERMES_DEVICE_DEVICE_SHELL": "zsh", "TALARIA_DEVICE_SHELL": "fish"}
	c := configulator.New(config.ConfigSchema()).
		WithEnvironmentVariables(&configulator.EnvironmentVariableOptions{Prefix: config.EnvPrefix, Separator: config.EnvSeparator}).
		WithEnviron(config.LegacyEnviron(func(k string) (string, bool) { v, ok := env[k]; return v, ok }))
	cfg, err := c.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hub.URL != hubURL || cfg.Device.Shell != "fish" {
		t.Errorf("hub.url %q, device.shell %q: want the legacy name read and the new one to win", cfg.Hub.URL, cfg.Device.Shell)
	}
}

func TestCommandMemory(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]struct {
		n    int64
		auto bool
	}{
		"auto": {0, true}, "AUTO": {0, true}, "off": {0, false}, "0": {0, false},
		"4G": {4 << 30, false}, "4GiB": {4 << 30, false}, "512m": {512 << 20, false}, "1t": {1 << 40, false},
		"20000000": {20000000, false}, "16M": {16 << 20, false},
	} {
		n, auto, err := config.Device{CommandMemoryMax: in}.CommandMemory()
		if err != nil || n != want.n || auto != want.auto {
			t.Errorf("%q: %d, %v, %v", in, n, auto, err)
		}
	}
	for _, in := range []string{"", "-1G", "1.5G", "4X", "15M", "99999999999T", "lots"} {
		if _, _, err := (config.Device{CommandMemoryMax: in}).CommandMemory(); !errors.Is(err, config.ErrMemorySize) {
			t.Errorf("%q: %v", in, err)
		}
	}
	cfg, err := load(map[string]string{hubURLEnv: hubURL}, "")
	if err != nil || cfg.Device.CommandMemoryMax != "auto" {
		t.Errorf("default %+v, %v", cfg, err)
	}
	if _, err := load(map[string]string{hubURLEnv: hubURL, "TALARIA_DEVICE_COMMAND_MEMORY_MAX": "big"}, ""); !errors.Is(err, config.ErrMemorySize) {
		t.Errorf("bad value: %v", err)
	}
}
