// Package cmd is the hermes-hub command.
package cmd

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/agents"
	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/device"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/USA-RedDragon/hermes-hub/internal/leader"
	"github.com/USA-RedDragon/hermes-hub/internal/memory"
	"github.com/USA-RedDragon/hermes-hub/internal/pdf"
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

	dialers := map[string]*gateway.Dialer{}
	for _, b := range cfg.Backends {
		dialers[b.ID] = gateway.NewDialer(gateway.Credentials{BaseURL: b.URL, Provider: b.Provider, Username: b.Username, Password: b.Password})
	}
	opts := service.Options{HoldTimeout: cfg.Hold.Timeout, Dialer: func(b config.Backend) service.Dialer { return dialers[b.ID] }}
	if st.Postgres() {
		opts.Relay = st
	}
	if r := pdf.New("", 0); r.Available() {
		opts.PDF = r
	} else {
		slog.Warn("poppler-utils is not installed, so attached PDFs go to the agent as plain files")
	}
	svc := service.New(cfg.Backends, st, opts)
	defer svc.Close()

	webOpts := web.Options{Service: svc, Links: st, Tokens: st, Groups: st}
	devices, err := newDevices(cfg, st, svc, dialers)
	if err != nil {
		return err
	}
	if devices != nil {
		svc.SetFiles(devices)
		webOpts.Devices, webOpts.DeviceAPI, webOpts.OnRevoke = st, devices.Handler(), devices.CloseDevice
		webOpts.Sandboxes, webOpts.Computers = devices.Sandboxes, devices
	}
	if devices != nil {
		router := agents.New(agents.Options{Backends: cfg.Backends, Sessions: svc, Places: devices, Store: st,
			Plugin: func(id string) agents.Plugin {
				if p := devices.Plugin(id); p != nil {
					return p
				}
				return nil
			}})
		go router.Run(ctx)
	}
	if webOpts.Memory, err = newMemory(cfg, svc, dialers); err != nil {
		return err
	}
	ui, err := web.New(cfg, webOpts)
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
		topts := telegram.Options{PublicURL: cfg.HTTP.PublicURL, Service: svc, Links: st, Tokens: st, State: st, Logger: slog.Default()}
		if devices != nil {
			topts.Computers = devices
		}
		bot, err := telegram.New(telegram.Config(cfg.Telegram), topts)
		if err != nil {
			return fmt.Errorf("failed to start Telegram bot: %w", err)
		}
		go runBot(botCtx, st, bot, cfg.Telegram, botDone)
	} else {
		close(botDone)
	}
	ui.SetReady(true)

	<-ctx.Done()
	stop()
	if err := shutdown(cfg.Shutdown, ui, svc, devices, srv, stopBot, botDone); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	slog.Info("Shutdown complete")
	return nil
}

// newDevices returns the hermes-device server, or nil when
// device.enabled is off. Only backends with devices on get device legs.
func newDevices(cfg *config.Config, st *store.Store, svc *service.Service, dialers map[string]*gateway.Dialer) (*device.Server, error) {
	if !cfg.Device.Enabled {
		return nil, nil //nolint:nilnil // nil means devices are off
	}
	key, err := cfg.Device.PrivateKey()
	if err != nil {
		return nil, err
	}
	var backends []config.Backend
	ds := map[string]device.Dialer{}
	for _, b := range cfg.Backends {
		if b.Devices {
			backends = append(backends, b)
			ds[b.ID] = dialers[b.ID]
		}
	}
	srv, err := device.New(device.Options{Store: st, Access: svc, Sessions: svc, Backends: backends, Dialers: ds, Key: key,
		PublicURL: cfg.HTTP.PublicURL, Config: cfg.Device})
	if err != nil {
		return nil, err
	}
	pub, _ := key.Public().(ed25519.PublicKey)
	slog.Info("Devices enabled", "backends", len(backends), "public_key", base64.StdEncoding.EncodeToString(pub))
	return srv, nil
}

// newMemory returns the Memory page's core, or nil when no backend has
// memory: every backend's Hermes may have the hermes-device plugin's
// memory routes, so each one counts.
func newMemory(cfg *config.Config, svc *service.Service, dialers map[string]*gateway.Dialer) (hub.Memory, error) {
	if len(cfg.Backends) == 0 {
		return nil, nil //nolint:nilnil // nil means no Memory page
	}
	rs := make(map[string]device.Requester, len(dialers))
	for id, d := range dialers {
		rs[id] = d
	}
	return memory.New(memory.Options{Access: svc, Backends: cfg.Backends, Requesters: rs})
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

// shutdown fails /readyz, keeps serving for the drain delay, gives up on
// held prompts, ends the event streams so browsers reconnect to another
// replica, stops the Telegram bot, which releases its leader lock, and
// stops the HTTP server. The steps after the drain delay share one
// timeout.
func shutdown(cfg config.Shutdown, ui *web.Server, svc *service.Service, devices *device.Server, srv *server.Server,
	stopBot context.CancelFunc, botDone <-chan struct{}) error {
	slog.Info("Shutting down: not ready, draining", "delay", cfg.DrainDelay)
	ui.SetReady(false)
	time.Sleep(cfg.DrainDelay)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	svc.Drain()
	ui.Drain()
	if devices != nil {
		devices.Drain()
	}
	stopBot()
	select {
	case <-botDone:
	case <-ctx.Done():
		slog.Warn("Telegram bot did not stop in time")
	}
	return srv.Stop(ctx)
}
