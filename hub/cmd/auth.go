package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
)

// prepareAuth fills what the config may leave to the hub: a session key
// kept in the database, and, while a local hub has no account, a setup
// token printed to the log.
func prepareAuth(ctx context.Context, cfg *config.Config, st *store.Store) error {
	if cfg.Session.Secret == "" {
		secret, err := st.SessionSecret(ctx)
		if err != nil {
			return fmt.Errorf("session secret: %w", err)
		}
		cfg.Session.Secret = secret
	}
	slog.Info("Login", "mode", cfg.Auth.Mode)
	if cfg.Auth.Mode != config.AuthLocal {
		return nil
	}
	n, err := st.CountAccounts(ctx)
	if err != nil {
		return fmt.Errorf("count accounts: %w", err)
	}
	if n > 0 {
		return nil
	}
	setup := cfg.HTTP.PublicURL + "/setup"
	if cfg.Auth.Local.SetupToken != "" {
		slog.Warn("The hub has no account yet. Open the setup page and give auth.local.setup_token", "url", setup)
		return nil
	}
	tok, err := st.NewSetupToken(ctx)
	if err != nil {
		return fmt.Errorf("setup token: %w", err)
	}
	slog.Warn("The hub has no account yet. Open the setup page and give this setup token. It works until the first account exists; a restart makes a new one",
		"url", setup, "setup_token", tok)
	return nil
}
