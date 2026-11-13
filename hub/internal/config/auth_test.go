package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
)

const (
	publicURL     = "https://hub.example.test"
	envPublicURL  = "HUB_HTTP_PUBLIC_URL"
	envSigningKey = "HUB_DEVICE_SIGNING_KEY"
)

// homelabFile is shaped like a deployed OIDC hub that sets everything but
// users and backends through environment variables: two people, one
// backend each, with devices and Honcho off.
const homelabFile = `
users:
  - sub: 9f8e7d6c5b4a
    name: alice
  - sub: 1a2b3c4d5e6f
    name: bob
backends:
  - id: alice
    name: Alice
    url: http://hermes-alice.hermes.svc:9120
    username: hub
    password: pw
    users: [alice]
    devices: true
    acp_url: http://hermes-alice.hermes.svc:9130
    service_devices:
      - id: sandbox
        token: hdt_0123456789abcdef0123456789abcdef
  - id: bob
    url: http://hermes-bob.hermes.svc:9120
    username: hub
    password: pw
    users: [bob]
`

func homelabEnv() map[string]string {
	return map[string]string{
		"HUB_LOG_LEVEL": "info", "HUB_HTTP_PORT": "8080", envPublicURL: publicURL,
		"HUB_HTTP_METRICS_ENABLED": valTrue,
		"HUB_OIDC_ISSUER_URL":      "https://auth.example.test/application/o/hub/",
		"HUB_OIDC_CLIENT_ID":       "hub-client", envOIDCClient: "cs", envSessionKey: strings.Repeat("s", 40),
		"HUB_UI_DEFAULT_BACKENDS": "alice=alice,bob=bob", "HUB_STORE_DRIVER": "postgres",
		"HUB_STORE_DSN": "postgres://hub@db.example.test/hub", "HUB_TELEGRAM_ENABLED": valTrue,
		"HUB_TELEGRAM_TOKEN": "123:abc", "HUB_TELEGRAM_DEFAULT_BACKENDS": "alice=alice,bob=bob",
		envDeviceEnabled: valTrue, "HUB_DEVICE_TRUSTED_PROXIES": "172.17.0.0/16", envSigningKey: testKey,
		"HUB_CLAUDE_ADMINS": "alice=alice", "HUB_USAGE_PROMETHEUS_URL": "http://prometheus.monitoring.svc:9090",
	}
}

func TestHomelabOIDCConfigUnchanged(t *testing.T) {
	t.Parallel()
	cfg, err := load(t, homelabEnv(), homelabFile)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Mode != config.AuthOIDC || cfg.OIDC.AutoCreate || len(cfg.Backends) != 2 || cfg.Backends[0].Everyone {
		t.Fatalf("auth %+v oidc %+v backends %d", cfg.Auth, cfg.OIDC, len(cfg.Backends))
	}
	if u := cfg.UserMap(); u["9f8e7d6c5b4a"] != userAlice || u["1a2b3c4d5e6f"] != "bob" {
		t.Fatalf("users = %v", u)
	}
	env := homelabEnv()
	env["HUB_CLAUDE_ADMINS"] = "carol=alice"
	if _, err := load(t, env, homelabFile); !errors.Is(err, config.ErrUser) {
		t.Fatalf("unknown user in claude.admins: %v", err)
	}
	if _, err := load(t, homelabEnv(), strings.Replace(homelabFile, "users: [bob]", "users: [carol]", 1)); !errors.Is(err, config.ErrUser) {
		t.Fatalf("unknown user in backends: %v", err)
	}
}

func TestLocalModeIsTheDefault(t *testing.T) {
	t.Parallel()
	env := map[string]string{envPublicURL: publicURL}
	cfg, err := load(t, env, "")
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Auth
	if a.Mode != config.AuthLocal || a.Local.MaxFailures != 5 || a.Local.Lockout.Minutes() != 15 || cfg.Session.Secret != "" {
		t.Fatalf("auth = %+v", a)
	}
	env["HUB_AUTH_ADMINS"] = userAlice
	env["HUB_CLAUDE_ADMINS"] = "alice=desktop"
	body := "backends:\n  - id: desktop\n    url: http://desktop:9120\n    username: hub\n    password: pw\n    users: [alice]\n"
	if _, err := load(t, env, body); err != nil {
		t.Fatalf("local accounts named in settings: %v", err)
	}
	env["HUB_AUTH_MODE"] = "ldap"
	if _, err := load(t, env, ""); !errors.Is(err, config.ErrAuthMode) {
		t.Fatalf("unknown mode: %v", err)
	}
}

func TestHermesShortcut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	if err := os.WriteFile(pw, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{envPublicURL: publicURL, "HUB_HERMES_URL": "http://hermes:9120/",
		"HUB_HERMES_USERNAME": "hub", "HUB_HERMES_PASSWORD_FILE": pw}
	cfg, err := load(t, env, "")
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Backends
	if len(b) != 1 || b[0].ID != config.HermesBackendID || b[0].Name != "Hermes" || b[0].URL != "http://hermes:9120" ||
		b[0].Password != "from-file" || !b[0].Everyone || b[0].Provider != "basic" {
		t.Fatalf("backends = %+v", b)
	}
	delete(env, "HUB_HERMES_PASSWORD_FILE")
	if _, err := load(t, env, ""); !errors.Is(err, config.ErrBackend) {
		t.Fatalf("no password: %v", err)
	}
	env["HUB_HERMES_PASSWORD"] = "pw"
	dup := "backends:\n  - id: hermes\n    url: http://other:9120\n    username: hub\n    password: pw\n"
	if _, err := load(t, env, dup); !errors.Is(err, config.ErrBackend) {
		t.Fatalf("duplicate id: %v", err)
	}
}

func TestSecretFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	env := homelabEnv()
	delete(env, envOIDCClient)
	env["HUB_SESSION_SECRET"] = "ignored"
	env["HUB_OIDC_CLIENT_SECRET_FILE"] = write("cs", "client\n")
	env["HUB_SESSION_SECRET_FILE"] = write("ss", strings.Repeat("k", 32))
	env["HUB_STORE_DSN_FILE"] = write("dsn", "postgres://hub@file.example.test/hub")
	env["HUB_TELEGRAM_TOKEN_FILE"] = write("tg", "456:def")
	body := strings.Replace(homelabFile, "    password: pw\n    users: [bob]", "    password_file: "+write("bob", "bob-pw")+"\n    users: [bob]", 1)
	body = strings.Replace(body, "token: hdt_0123456789abcdef0123456789abcdef", "token_file: "+write("dev", "hdt_"+strings.Repeat("f", 32)), 1)
	cfg, err := load(t, env, body)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OIDC.ClientSecret != "client" || cfg.Session.Secret != strings.Repeat("k", 32) || cfg.Store.DSN != "postgres://hub@file.example.test/hub" ||
		cfg.Telegram.Token != "456:def" || cfg.Backends[1].Password != "bob-pw" || cfg.Backends[0].ServiceDevices[0].Token != "hdt_"+strings.Repeat("f", 32) {
		t.Fatalf("secrets not read from files: %+v", cfg)
	}
	env["HUB_TELEGRAM_TOKEN_FILE"] = filepath.Join(dir, "missing")
	if _, err := load(t, env, body); !errors.Is(err, config.ErrSecretFile) {
		t.Fatalf("missing file: %v", err)
	}
	env["HUB_TELEGRAM_TOKEN_FILE"] = write("empty", "\n")
	if _, err := load(t, env, body); !errors.Is(err, config.ErrSecretFile) {
		t.Fatalf("empty file: %v", err)
	}
}

func TestProxyAndOIDCSettings(t *testing.T) {
	t.Parallel()
	base := func(kv ...string) map[string]string {
		env := map[string]string{envPublicURL: publicURL}
		for i := 0; i+1 < len(kv); i += 2 {
			env[kv[i]] = kv[i+1]
		}
		return env
	}
	for _, c := range []struct {
		env  map[string]string
		want error
	}{
		{base("HUB_AUTH_MODE", "proxy"), config.ErrProxy},
		{base("HUB_AUTH_MODE", "proxy", "HUB_AUTH_TRUSTED_PROXIES", "not-a-cidr"), config.ErrProxy},
		{base("HUB_AUTH_MODE", "proxy", "HUB_AUTH_TRUSTED_PROXIES", "10.0.0.0/8"), nil},
		{base("HUB_AUTH_MODE", "proxy", "HUB_AUTH_PROXY_JWT_JWKS_URL", "https://idp.example.test/jwks"), config.ErrProxy},
		{base("HUB_AUTH_MODE", "proxy", "HUB_AUTH_PROXY_JWT_JWKS_URL", "https://idp.example.test/jwks",
			"HUB_AUTH_PROXY_JWT_ISSUER", "https://idp.example.test"), nil},
		{base("HUB_AUTH_MODE", "proxy", "HUB_AUTH_TRUSTED_PROXIES", "127.0.0.1", "HUB_AUTH_PROXY_LOGOUT_URL", "javascript:x"), config.ErrProxy},
		{base("HUB_AUTH_MODE", "oidc"), config.ErrOIDC},
		{base("HUB_OIDC_ISSUER_URL", "https://idp.example.test", "HUB_OIDC_CLIENT_ID", "hub", envOIDCClient, "cs",
			"HUB_OIDC_AUTO_CREATE", valTrue), config.ErrOIDC},
		{base("HUB_OIDC_ISSUER_URL", "https://idp.example.test", "HUB_OIDC_CLIENT_ID", "hub", envOIDCClient, "cs",
			"HUB_OIDC_AUTO_CREATE", valTrue, "HUB_OIDC_ALLOWED_DOMAINS", "example.test", "HUB_AUTH_ADMINS", "anyone"), nil},
	} {
		cfg, err := load(t, c.env, "")
		if !errors.Is(err, c.want) || (c.want == nil && err != nil) {
			t.Errorf("%v: err = %v, want %v", c.env, err, c.want)
			continue
		}
		if err == nil && cfg.Auth.Mode == config.AuthProxy && cfg.Auth.Proxy.JWT.JWKSURL != "" && cfg.Auth.Proxy.JWT.Header != "Authorization" {
			t.Errorf("jwt header default = %q", cfg.Auth.Proxy.JWT.Header)
		}
	}
}
