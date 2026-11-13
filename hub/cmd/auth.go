package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
)

// prepareAuth fills what the config may leave to the hub: a session key
// kept in the database, and, while a local hub has no account, a warning
// with the setup URL in the log.
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
	slog.Warn("Setup is not finished. The first visitor of the setup page becomes the admin, so finish setup before the hub is open to the internet",
		"url", cfg.HTTP.PublicURL+"/setup")
	return nil
}
