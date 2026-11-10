package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/USA-RedDragon/configulator/v2"
	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/goccy/go-yaml"
)

const file = `
http:
  public_url: https://hub.example.test/
oidc:
  issuer_url: https://auth.example.test/application/o/hermes-hub/
  client_id: hub
backends:
  - id: desktop
    url: http://desktop:9120/
    username: hub
    password: pw
    users: [alice]
  - id: cluster
    name: Cluster
    url: http://hermes.hermes.svc:9120
    username: hub
    password: pw
    groups: [family]
    show_reasoning: false
`

func load(t *testing.T, env map[string]string, body string) (*config.Config, error) {
	t.Helper()
	c := configulator.New(config.ConfigSchema()).
		WithEnvironmentVariables(&configulator.EnvironmentVariableOptions{Prefix: config.EnvPrefix, Separator: config.EnvSeparator}).
		WithFile(&configulator.FileOptions{Search: []string{"config.yaml"}, Decoders: configulator.Decoders{".yaml": yaml.Unmarshal}}).
		WithEnviron(func(k string) (string, bool) { v, ok := env[k]; return v, ok }).
		WithReadFile(func(string) ([]byte, error) { return []byte(body), nil })
	return c.Load()
}

//nolint:gosec // environment variable names, not credentials
const (
	envClientSecret  = "HUB_OIDC_CLIENT_SECRET"
	envSessionSecret = "HUB_SESSION_SECRET"
)

func secrets() map[string]string {
	return map[string]string{envClientSecret: "cs", envSessionSecret: strings.Repeat("x", 32)}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, secrets(), file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP.PublicURL != "https://hub.example.test" || cfg.Store.Path != "/data/hermes-hub.db" {
		t.Fatalf("http/store = %+v %+v", cfg.HTTP, cfg.Store)
	}
	b := cfg.Backends
	if len(b) != 2 || b[0].Name != "desktop" || b[0].Provider != "basic" || b[0].URL != "http://desktop:9120" ||
		!*b[0].ShowReasoning || *b[1].ShowReasoning || b[1].Groups[0] != "family" {
		t.Fatalf("backends = %+v", b)
	}
	if cfg.Telegram.Enabled {
		t.Fatal("telegram on by default")
	}
}

func TestLoadErrors(t *testing.T) {
	t.Parallel()
	var req *configulator.RequiredError
	if _, err := load(t, map[string]string{}, file); !errors.As(err, &req) {
		t.Fatalf("missing secrets: %v", err)
	}
	short := map[string]string{envClientSecret: "cs", envSessionSecret: "short"}
	if _, err := load(t, short, file); !errors.Is(err, config.ErrShortSecret) {
		t.Fatalf("short secret: %v", err)
	}
	for _, bad := range []string{
		strings.Replace(file, "id: cluster", "id: desktop", 1),
		strings.Replace(file, "http://desktop:9120/", "desktop", 1),
		strings.Replace(file, "    password: pw\n    users", "    users", 1),
		strings.Replace(file, "https://hub.example.test/", "https://hub.example.test/sub", 1),
	} {
		if _, err := load(t, secrets(), bad); err == nil {
			t.Fatalf("accepted:\n%s", bad)
		}
	}
	tg := secrets()
	tg["HUB_TELEGRAM_ENABLED"] = "true"
	if _, err := load(t, tg, file); err == nil {
		t.Fatal("telegram enabled without token accepted")
	}
}
