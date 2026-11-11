package config_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/configulator/v2"
	"github.com/USA-RedDragon/hermes-device/assertion"
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

const (
	defaultDB        = "/data/hermes-hub.db"
	envDeviceEnabled = "HUB_DEVICE_ENABLED"
	valTrue          = "true"
)

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
	tg["HUB_TELEGRAM_ENABLED"] = valTrue
	if _, err := load(t, tg, file); err == nil {
		t.Fatal("telegram enabled without token accepted")
	}
}

// testKey is a throwaway key from openssl genpkey -algorithm ed25519, and
// testPub its openssl pkey -pubout.
//
//nolint:gosec // a test key that signs nothing real
const (
	testKey = "-----BEGIN PRIVATE KEY-----\nMC4CAQAwBQYDK2VwBCIEIMWpXNQfEB+9EILo9tcvcyxQjkZ4nmD9etkP6FFzgPbL\n-----END PRIVATE KEY-----\n"
	testPub = "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAFdRPWzV70Dz3asm11xo9SeU/pArIg5dNZX+Vo9uYh7g=\n-----END PUBLIC KEY-----\n"
)

func TestDeviceSettings(t *testing.T) {
	t.Parallel()
	env := secrets()
	env[envDeviceEnabled] = valTrue
	env["HUB_DEVICE_SIGNING_KEY"] = testKey
	env["HUB_DEVICE_TRUSTED_PROXIES"] = "10.0.0.0/8,192.0.2.1"
	body := file + `    devices: true
    acp_url: http://hermes.hermes.svc:9130/
`
	cfg, err := load(t, env, body)
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Device
	if d.PairingTTL != 10*time.Minute || d.PollInterval != 5*time.Second || d.RecheckInterval != 10*time.Second {
		t.Fatalf("defaults = %+v", d)
	}
	if !cfg.Backends[1].Devices || cfg.Backends[1].ACPURL != "http://hermes.hermes.svc:9130" || cfg.Backends[0].Devices {
		t.Fatalf("backends = %+v", cfg.Backends)
	}
	key, err := d.PrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	want, err := assertion.ParsePublicKeys(testPub)
	if err != nil {
		t.Fatal(err)
	}
	if pub, _ := key.Public().(ed25519.PublicKey); !pub.Equal(want[0]) {
		t.Fatal("public key does not match openssl's")
	}
	if p, err := d.Proxies(); err != nil || len(p) != 2 || p[1].Bits() != 32 {
		t.Fatalf("proxies = %v %v", p, err)
	}
	seed := base64.StdEncoding.EncodeToString(key.Seed())
	if k, err := config.ParseSigningKey(seed); err != nil || !k.Equal(key) {
		t.Fatalf("seed = %v", err)
	}
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := (config.Device{SigningKeyFile: path, SigningKey: "ignored"}).PrivateKey(); err != nil || !k.Equal(key) {
		t.Fatalf("key file = %v", err)
	}
	for _, bad := range []map[string]string{
		{envDeviceEnabled: valTrue},
		{envDeviceEnabled: valTrue, "HUB_DEVICE_SIGNING_KEY": testPub},
		{envDeviceEnabled: valTrue, "HUB_DEVICE_SIGNING_KEY": "c2hvcnQ="},
		{"HUB_DEVICE_TRUSTED_PROXIES": "not-a-cidr"},
	} {
		env := secrets()
		for k, v := range bad {
			env[k] = v
		}
		if _, err := load(t, env, file); !errors.Is(err, config.ErrDevice) {
			t.Fatalf("%v: err = %v", bad, err)
		}
	}
}

func TestServiceDevices(t *testing.T) {
	t.Parallel()
	ok := file + `    service_devices:
      - id: sandbox-bob
        token: hdt_0123456789abcdef0123456789abcdef
`
	cfg, err := load(t, secrets(), ok)
	if err != nil {
		t.Fatal(err)
	}
	if sd := cfg.Backends[1].ServiceDevices; len(sd) != 1 || sd[0].Name != "sandbox-bob" {
		t.Fatalf("service devices = %+v", sd)
	}
	for _, bad := range []string{
		"      - id: d_0123\n        token: hdt_0123456789abcdef0123456789abcdef\n",
		"      - id: sandbox-bob\n        token: short\n",
		"      - id: sandbox-bob\n        token: hdt_0123456789abcdef0123456789abcdef\n      - id: sandbox-bob\n        token: hdt_0123456789abcdef0123456789abcdeg\n",
	} {
		if _, err := load(t, secrets(), file+"    service_devices:\n"+bad); !errors.Is(err, config.ErrBackend) {
			t.Fatalf("%q: err = %v", bad, err)
		}
	}
}

func TestMediaRoots(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, secrets(), file+"    media_roots: [/opt/data/hermes/cache, /srv/out]\n")
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.Backends[1].MediaRoots; len(r) != 2 || r[1] != "/srv/out" || len(cfg.Backends[0].MediaRoots) != 0 {
		t.Fatalf("media roots = %v / %v", cfg.Backends[0].MediaRoots, r)
	}
	for _, bad := range []string{"/", "relative", "/a/../b", "/a/"} {
		if _, err := load(t, secrets(), file+"    media_roots: ["+bad+"]\n"); !errors.Is(err, config.ErrBackend) {
			t.Errorf("media root %q: %v", bad, err)
		}
	}
}
