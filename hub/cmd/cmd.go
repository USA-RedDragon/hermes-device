// Package cmd is the talaria-hub command.
package cmd

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/agents"
	"github.com/USA-RedDragon/talaria/hub/internal/claude"
	"github.com/USA-RedDragon/talaria/hub/internal/claudecode"
	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/insights"
	"github.com/USA-RedDragon/talaria/hub/internal/leader"
	"github.com/USA-RedDragon/talaria/hub/internal/memory"
	"github.com/USA-RedDragon/talaria/hub/internal/notify"
	"github.com/USA-RedDragon/talaria/hub/internal/pdf"
	"github.com/USA-RedDragon/talaria/hub/internal/scheduled"
	"github.com/USA-RedDragon/talaria/hub/internal/server"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/subagents"
	"github.com/USA-RedDragon/talaria/hub/internal/telegram"
	"github.com/USA-RedDragon/talaria/hub/internal/web"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"
)

// NewCommand returns the root command.
func NewCommand(version, commit string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "talaria-hub",
		Short:   "Talaria hub: one authenticated web front end for many Hermes Agent gateways",
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
	slog.Info("Talaria hub", "version", cmd.Annotations["version"], "commit", cmd.Annotations["commit"])
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
	if err := prepareAuth(ctx, cfg, st); err != nil {
		return err
	}
	opts := service.Options{HoldTimeout: cfg.Hold.Timeout, Dialer: func(b config.Backend) service.Dialer { return dialers[b.ID] },
		Answers: st, Grants: st}
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

	notifier, err := newNotifier(cfg, st, svc, dialers)
	if err != nil {
		return err
	}
	svc.SetNotify(notifier.Notify)
	webOpts := web.Options{Service: svc, Links: st, Tokens: st, Groups: st, Notices: st, Notifier: notifier, Chats: st, Prompts: st, Partners: st, Accounts: st,
		Version: cmd.Annotations["version"]}
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
	if devices != nil {
		cc, err := newClaudeCode(cfg, st, svc, devices)
		if err != nil {
			return err
		}
		devices.SetUsage(cc)
		webOpts.ClaudeCode = cc
		go cc.Run(ctx)
	}
	if webOpts.Memory, err = newMemory(cfg, svc, dialers); err != nil {
		return err
	}
	if devices != nil {
		webOpts.Files = devices
	}
	webOpts.Insights = newInsights(cfg, svc, webOpts.Memory, dialers)
	cs := newClaude(cfg, svc, notifier, dialers)
	if cs != nil {
		svc.SetTurnErrors(cs.TurnFailed)
		webOpts.Claude = cs
	}
	if sc := newScheduled(cfg, svc, st, dialers); sc != nil {
		webOpts.Scheduled = sc
	}
	sa := newSubagents(cfg, svc, dialers)
	if sa != nil {
		webOpts.Subagents = sa
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
	notifyOn := false
	if cfg.Telegram.Enabled {
		topts := telegram.Options{PublicURL: cfg.HTTP.PublicURL, Service: svc, Links: st, Tokens: st, State: st, Notices: st,
			Prompts: st, Pins: st, Logger: slog.Default()}
		if devices != nil {
			topts.Computers = devices
		}
		if cs != nil {
			topts.Claude = cs
		}
		if sa != nil {
			topts.Subagents = sa
		}
		bot, err := telegram.New(telegram.Config(cfg.Telegram), topts)
		if err != nil {
			return fmt.Errorf("failed to start Telegram bot: %w", err)
		}
		notifier.SetTelegram(bot)
		notifyOn = true
		go runBot(botCtx, st, bot, cfg.Telegram, botDone)
	} else {
		close(botDone)
	}
	notifyOn = notifyOn || notifier.PushKey() != ""
	notifyCtx, stopNotify := context.WithCancel(context.Background())
	defer stopNotify()
	if notifyOn {
		go notifier.Run(notifyCtx)
		go runNotifyLeader(notifyCtx, st, svc, notifier, cfg.Notify)
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

// newNotifier returns the notifier of notices on people's phones.
func newNotifier(cfg *config.Config, st *store.Store, svc *service.Service, dialers map[string]*gateway.Dialer) (*notify.Notifier, error) {
	key, err := cfg.Notify.PushKey()
	if err != nil {
		return nil, err
	}
	users := make([]string, 0, len(cfg.Users))
	for _, u := range cfg.Users {
		users = append(users, u.Name)
	}
	names := map[string]string{}
	jobs := map[string]notify.Requester{}
	for _, b := range cfg.Backends {
		names[b.ID] = b.Name
		jobs[b.ID] = dialers[b.ID]
	}
	opts := notify.Options{Store: st, Access: svc, Prompts: svc, Users: users, Backends: names, Jobs: jobs,
		PublicURL: cfg.HTTP.PublicURL, PushSubject: cfg.Notify.VAPIDSubject, Remind: cfg.Notify.RemindAfter, JobPoll: cfg.Notify.JobInterval,
		Accounts: st.AccountNames}
	if key != nil && cfg.Notify.VAPIDSubject == "" {
		slog.Warn("Web Push disabled: set notify.vapid_subject to a mailto: or https: URL, or use an https http.public_url")
	} else if key != nil {
		opts.Push = key
		slog.Info("Web Push enabled", "public_key", key.PublicKey())
	}
	return notify.New(opts), nil
}

// notifyRetry is how often a replica that does not lead tries to take
// over the notifier's leader work.
const notifyRetry = 5 * time.Second

// runNotifyLeader runs, in one replica, the watch of running chats, the
// reminders and the reads of scheduled jobs.
func runNotifyLeader(ctx context.Context, st *store.Store, svc *service.Service, n *notify.Notifier, cfg config.Notify) {
	log := slog.Default().With("component", "notify")
	try := func(ctx context.Context) (leader.Lease, error) {
		l, err := st.TryLock(ctx, leader.Key("notify"), botCheckInterval)
		if l == nil {
			return nil, err
		}
		return l, nil
	}
	leader.Run(ctx, try, notifyRetry, log, func(ctx context.Context) {
		go svc.Watch(ctx, cfg.WatchInterval)
		n.Lead(ctx)
	})
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

// newClaudeCode returns the core of the Usage page's Claude Code panel.
// It needs devices: computers send their usage on the device API, and the
// hub asks each acp-host with a device assertion.
func newClaudeCode(cfg *config.Config, st *store.Store, svc *service.Service, devices *device.Server) (*claudecode.Service, error) {
	backends := make([]claudecode.Backend, 0, len(cfg.Backends))
	for _, b := range cfg.Backends {
		backends = append(backends, claudecode.Backend{ID: b.ID, Name: b.Name, Hermes: b.Devices && b.ACPURL != ""})
	}
	return claudecode.New(claudecode.Options{Store: st, Access: svc, Devices: st, Backends: backends, Fetch: devices.HermesUsage,
		Location: cfg.Claude.Location()})
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

// newInsights returns the core of chat search and the Usage page, or nil
// when there is no backend.
func newInsights(cfg *config.Config, svc *service.Service, mem hub.Memory, dialers map[string]*gateway.Dialer) hub.Insights {
	if len(cfg.Backends) == 0 {
		return nil
	}
	rs := make(map[string]device.Requester, len(dialers))
	for id, d := range dialers {
		rs[id] = d
	}
	opts := insights.Options{Access: svc, Requesters: rs, Observer: svc}
	if b, ok := mem.(insights.Backlogs); ok {
		opts.Backlogs = b
	}
	if cfg.Usage.PrometheusURL != "" {
		opts.GPU = insights.NewGPU(cfg.Usage.PrometheusURL, &http.Client{Timeout: 15 * time.Second})
	}
	return insights.New(opts)
}

// newClaude returns the Claude accounts core, or nil when there is no
// backend. Any backend's Hermes may have the Claude plugin.
func newClaude(cfg *config.Config, svc *service.Service, n *notify.Notifier, dialers map[string]*gateway.Dialer) *claude.Service {
	if len(cfg.Backends) == 0 {
		return nil
	}
	rs := make(map[string]device.Requester, len(dialers))
	for id, d := range dialers {
		rs[id] = d
	}
	admins, err := cfg.Claude.AdminMap()
	if err != nil {
		return nil
	}
	return claude.New(claude.Options{Access: svc, Admins: admins, Location: cfg.Claude.Location(), Requesters: rs,
		Notify: n.Notify, Announce: svc.Announce})
}

// newSubagents returns the core of the chats' Subagents setting, or nil
// when there is no backend. Any backend's Hermes may have the
// subagent-routes plugin.
func newSubagents(cfg *config.Config, svc *service.Service, dialers map[string]*gateway.Dialer) *subagents.Service {
	if len(cfg.Backends) == 0 {
		return nil
	}
	rs := make(map[string]device.Requester, len(dialers))
	for id, d := range dialers {
		rs[id] = d
	}
	return subagents.New(subagents.Options{Access: svc, Requesters: rs})
}

// newScheduled returns the Scheduled page's core, or nil when there is no
// backend. Every Hermes has the cron routes of its dashboard.
func newScheduled(cfg *config.Config, svc *service.Service, st *store.Store, dialers map[string]*gateway.Dialer) *scheduled.Service {
	if len(cfg.Backends) == 0 {
		return nil
	}
	rs := make(map[string]scheduled.Requester, len(dialers))
	for id, d := range dialers {
		rs[id] = d
	}
	return scheduled.New(scheduled.Options{Access: svc, Backends: cfg.Backends, Requesters: rs, Devices: st,
		Location: cfg.UI.Location()})
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
