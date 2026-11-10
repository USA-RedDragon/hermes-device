package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

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
users:
  - sub: 0a1b2c
    name: alice
  - sub: 3d4e5f
    name: bob
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

const defaultDB = "/data/hermes-hub.db"

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
	if cfg.HTTP.PublicURL != "https://hub.example.test" || cfg.Store.Path != defaultDB {
		t.Fatalf("http/store = %+v %+v", cfg.HTTP, cfg.Store)
	}
	b := cfg.Backends
	if len(b) != 2 || b[0].Name != "desktop" || b[0].Provider != "basic" || b[0].URL != "http://desktop:9120" ||
		!*b[0].ShowReasoning || *b[1].ShowReasoning || b[1].Groups[0] != "family" {
		t.Fatalf("backends = %+v", b)
	}
	if d, err := cfg.UI.DefaultBackendMap(); err != nil || len(d) != 0 {
		t.Fatalf("ui defaults = %v %v", d, err)
	}
	if u := cfg.UserMap(); u["0a1b2c"] != "alice" || u["3d4e5f"] != "bob" || len(u) != 2 {
		t.Fatalf("users = %v", u)
	}
	if cfg.Telegram.Enabled {
		t.Fatal("telegram on by default")
	}
	if cfg.Store.Driver != config.DriverSQLite || cfg.Shutdown.DrainDelay != 5*time.Second ||
		cfg.Shutdown.Timeout != 20*time.Second || cfg.Hold.Timeout != 10*time.Minute || cfg.Telegram.LeaderRetry != 2*time.Second {
		t.Fatalf("store/shutdown/hold defaults = %+v %+v %+v %v", cfg.Store, cfg.Shutdown, cfg.Hold, cfg.Telegram.LeaderRetry)
	}
}

func TestLoadPostgres(t *testing.T) {
	t.Parallel()
	env := secrets()
	env["HUB_STORE_DRIVER"] = "postgres"
	if _, err := load(t, env, file); !errors.Is(err, config.ErrStoreDSN) {
		t.Fatalf("postgres without dsn: %v", err)
	}
	env["HUB_STORE_DSN"] = "postgres://hub@db.example.com/hub"
	env["HUB_STORE_IMPORT_PATH"] = defaultDB
	cfg, err := load(t, env, file)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Store.DSN != "postgres://hub@db.example.com/hub" || cfg.Store.ImportPath != defaultDB {
		t.Fatalf("store = %+v", cfg.Store)
	}
	env["HUB_STORE_DRIVER"] = "mysql"
	if _, err := load(t, env, file); !errors.Is(err, config.ErrStoreDriver) {
		t.Fatalf("unknown driver: %v", err)
	}
	env = secrets()
	env["HUB_STORE_IMPORT_PATH"] = defaultDB
	if _, err := load(t, env, file); !errors.Is(err, config.ErrImportPath) {
		t.Fatalf("import with sqlite: %v", err)
	}
	env = secrets()
	env["HUB_SHUTDOWN_DRAIN_DELAY"] = "-1s"
	if _, err := load(t, env, file); !errors.Is(err, config.ErrDuration) {
		t.Fatalf("negative drain delay: %v", err)
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
		file + "ui:\n  default_backends: [bob]\n",
		file + "ui:\n  default_backends: [nobody=cluster]\n",
		strings.Replace(file, "users: [alice]", "users: [carol]", 1),
		strings.Replace(file, "sub: 3d4e5f", "sub: 0a1b2c", 1),
		strings.Replace(file, "name: bob", "name: alice", 1),
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
