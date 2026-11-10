// Package cmd is the hermes-hub command.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/leader"
	"github.com/USA-RedDragon/hermes-hub/internal/server"
	"github.com/USA-RedDragon/hermes-hub/internal/service"
	"github.com/USA-RedDragon/hermes-hub/internal/store"
	"github.com/USA-RedDragon/hermes-hub/internal/telegram"
	"github.com/USA-RedDragon/hermes-hub/internal/web"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
)

// NewCommand returns the root command.
func NewCommand(version, commit string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "hermes-hub",
		Short:   "One authenticated web front end for many Hermes Agent gateways",
		Version: fmt.Sprintf("%s - %s", version, commit),
		Annotations: map[string]string{
			"version": version,
			"commit":  commit,
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	loader := config.NewLoader(cmd.Flags())
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		cfg, err := loader.Load()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		return run(cmd, cfg)
	}
	return cmd
}

func logLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func run(cmd *cobra.Command, cfg *config.Config) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)})))
	slog.Info("Hermes Hub", "version", cmd.Annotations["version"], "commit", cmd.Annotations["commit"])
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()

	st, err := openStore(ctx, &cfg.Store)
	if err != nil {
		return err
	}
	defer st.Close()

	svc := service.New(cfg.Backends, st, service.Options{})
	defer svc.Close()

	ui, err := web.New(cfg, web.Options{Service: svc, Links: st, Tokens: st, Groups: st})
	if err != nil {
		return err
	}
	srv := server.New(&cfg.HTTP, ui.Handler(), prometheus.DefaultRegisterer)
	if err := srv.Start(ctx); err != nil {
		return fmt.Errorf("failed to start HTTP server: %w", err)
	}

	botCtx, stopBot := context.WithCancel(context.Background())
	defer stopBot()
	botDone := make(chan struct{})
	if cfg.Telegram.Enabled {
		bot, err := telegram.New(telegram.Config(cfg.Telegram), telegram.Options{PublicURL: cfg.HTTP.PublicURL, Service: svc,
			Links: st, Tokens: st, State: st, Logger: slog.Default()})
		if err != nil {
			return fmt.Errorf("failed to start Telegram bot: %w", err)
		}
		go runBot(botCtx, st, bot, cfg.Telegram, botDone)
	} else {
		close(botDone)
	}

	<-ctx.Done()
	slog.Info("Shutting down")
	stopBot()
	<-botDone
	if err := srv.Stop(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	slog.Info("Shutdown complete")
	return nil
}

func openStore(ctx context.Context, cfg *config.Store) (*store.Store, error) {
	if cfg.Driver != config.DriverPostgres {
		return store.Open(ctx, cfg.Path)
	}
	st, err := store.OpenPostgres(ctx, cfg.DSN)
	if err != nil {
		return nil, err
	}
	if cfg.ImportPath != "" {
		n, ran, err := st.ImportSQLite(ctx, cfg.ImportPath)
		if err != nil {
			_ = st.Close()
			return nil, err
		}
		if ran {
			slog.Info("Imported the SQLite store into PostgreSQL", "path", cfg.ImportPath, "rows", n)
		}
	}
	return st, nil
}

// botCheckInterval is how often the Telegram leader checks that it still
// holds its lock.
const botCheckInterval = 2 * time.Second

// runBot runs the Telegram bot in the one replica that holds the bot's
// leader lock. With SQLite there is one replica, and it always leads.
func runBot(ctx context.Context, st *store.Store, bot *telegram.Bot, cfg config.Telegram, done chan<- struct{}) {
	defer close(done)
	log := slog.Default().With("component", "telegram")
	key := leader.Key("telegram/" + strconv.FormatInt(telegram.BotID(cfg.Token), 10))
	try := func(ctx context.Context) (leader.Lease, error) {
		l, err := st.TryLock(ctx, key, botCheckInterval)
		if l == nil {
			return nil, err
		}
		return l, nil
	}
	leader.Run(ctx, try, cfg.LeaderRetry, log, func(ctx context.Context) {
		if err := bot.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("Telegram bot stopped", "error", err.Error())
		}
	})
}
