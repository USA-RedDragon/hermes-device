package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrSecretFile is a _file setting whose file cannot be read or is empty.
var ErrSecretFile = errors.New("cannot read secret file")

// secretFile is a secret setting and the _file setting that may hold it.
type secretFile struct {
	value         *string
	path, setting string
}

// read replaces the secret with the trimmed content of the file when the
// _file setting is set. A file wins over the value.
func (f secretFile) read() error {
	if f.path == "" {
		return nil
	}
	b, err := os.ReadFile(f.path)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrSecretFile, f.setting, err)
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return fmt.Errorf("%w: %s: %s is empty", ErrSecretFile, f.setting, f.path)
	}
	*f.value = text
	return nil
}

// readSecretFiles fills every secret that has a _file setting from its file.
func (c *Config) readSecretFiles() error {
	files := []secretFile{
		{&c.OIDC.ClientSecret, c.OIDC.ClientSecretFile, "oidc.client_secret_file"},
		{&c.Session.Secret, c.Session.SecretFile, "session.secret_file"},
		{&c.Store.DSN, c.Store.DSNFile, "store.dsn_file"},
		{&c.Telegram.Token, c.Telegram.TokenFile, "telegram.token_file"},
		{&c.Auth.Local.SetupToken, c.Auth.Local.SetupTokenFile, "auth.local.setup_token_file"},
		{&c.Hermes.Password, c.Hermes.PasswordFile, "hermes.password_file"},
	}
	for i := range c.Backends {
		b := &c.Backends[i]
		files = append(files, secretFile{&b.Password, b.PasswordFile, fmt.Sprintf("backends[%d].password_file", i)})
		for j := range b.ServiceDevices {
			d := &b.ServiceDevices[j]
			files = append(files, secretFile{&d.Token, d.TokenFile, fmt.Sprintf("backends[%d].service_devices[%d].token_file", i, j)})
		}
	}
	for _, f := range files {
		if err := f.read(); err != nil {
			return err
		}
	}
	return nil
}
