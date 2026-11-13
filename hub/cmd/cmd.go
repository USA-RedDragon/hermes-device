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
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/agentcfg"
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
	"github.com/USA-RedDragon/talaria/hub/internal/models"
	"github.com/USA-RedDragon/talaria/hub/internal/notify"
	"github.com/USA-RedDragon/talaria/hub/internal/pdf"
	"github.com/USA-RedDragon/talaria/hub/internal/scheduled"
	"github.com/USA-RedDragon/talaria/hub/internal/secretbox"
	"github.com/USA-RedDragon/talaria/hub/internal/server"
	"github.com/USA-RedDragon/talaria/hub/internal/service"
	"github.com/USA-RedDragon/talaria/hub/internal/store"
	"github.com/USA-RedDragon/talaria/hub/internal/subagents"
	"github.com/USA-RedDragon/talaria/hub/internal/telegram"
	"github.com/USA-RedDragon/talaria/hub/internal/updates"
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
		Answers: st, Grants: st, SubagentLog: st}
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
	webOpts := web.Options{Service: svc, Links: st, Tokens: st, Groups: st, Notices: st, Notifier: notifier, Chats: st, Prompts: st, Partners: st, AgentMessages: st, Accounts: st,
		Version: cmd.Annotations["version"], Started: time.Now(), Agent: newAgent(cfg, svc, dialers), Snapshot: st.Snapshot, Scratch: cfg.Store.ScratchDir()}
	var devices *device.Server
	if cfg.Device.Enabled {
		if devices, err = newDevices(cfg, st, svc, dialers); err != nil {
			return err
		}
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
	if len(cfg.Backends) > 0 {
		if webOpts.Memory, err = newMemory(cfg, svc, dialers); err != nil {
			return err
		}
	}
	if devices != nil {
		webOpts.Files = devices
	}
	cs := newClaude(cfg, svc, notifier, dialers)
	webOpts.Models = newModels(cfg, svc, cs, dialers)
	webOpts.Insights = newInsights(cfg, svc, webOpts.Memory, dialers)
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
	box, err := secretbox.New(st, cfg.Session.Secret)
	if err != nil {
		slog.Warn("The hub cannot keep secrets in its database, so Settings cannot set a Telegram token", "error", err.Error())
		box = nil
	}
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
	bots := newBotManager(cfg.Telegram, topts, st, box)
	webOpts.Telegram = bots
	if cfg.Updates.Check {
		uc := updates.New(cfg.Updates.URL, cmd.Annotations["version"], cfg.UI.Install, updateSources(cfg, st, devices), nil)
		webOpts.Updates = uc
		go uc.Run(ctx)
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
		cfgCopy := cfg.Telegram
		if _, err := telegram.New(telegram.Config(cfgCopy), topts); err != nil {
			return fmt.Errorf("failed to start Telegram bot: %w", err)
		}
	}
	notifier.SetTelegram(bots)
	go func() {
		defer close(botDone)
		bots.run(botCtx)
	}()
	// The notifier also serves a bot started later from Settings
	notifyOn := true
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
	key, pushOn, err := cfg.Notify.PushKey()
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
	if pushOn && cfg.Notify.VAPIDSubject == "" {
		slog.Warn("Web Push disabled: set notify.vapid_subject to a mailto: or https: URL, or use an https http.public_url")
	} else if pushOn {
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
	leader.Run(ctx, tryLock(st, leader.Key("notify")), notifyRetry, log, func(ctx context.Context) {
		go svc.Watch(ctx, cfg.WatchInterval)
		n.Lead(ctx)
	})
}

// newDevices returns the talaria device server, which device.enabled turns
// on. Only backends with devices on get device legs.
func newDevices(cfg *config.Config, st *store.Store, svc *service.Service, dialers map[string]*gateway.Dialer) (*device.Server, error) {
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

// newMemory returns the Memory page's core. Every backend's Hermes may
// have the talaria device plugin's memory routes, so each one counts.
func newMemory(cfg *config.Config, svc *service.Service, dialers map[string]*gateway.Dialer) (hub.Memory, error) {
	rs := make(map[string]device.Requester, len(dialers))
	for id, d := range dialers {
		rs[id] = d
	}
	return memory.New(memory.Options{Access: svc, Backends: cfg.Backends, Requesters: rs})
}

// newModels returns the core of Settings > Models, or nil when there is no
// backend. A backend whose Hermes has the Claude accounts plugin (cs says
// so) does not offer an Anthropic API key in Connect a model.
func newModels(cfg *config.Config, svc *service.Service, cs *claude.Service, dialers map[string]*gateway.Dialer) hub.ModelSetup {
	if len(cfg.Backends) == 0 {
		return nil
	}
	rs := make(map[string]device.Requester, len(dialers))
	for id, d := range dialers {
		rs[id] = d
	}
	opts := models.Options{Access: svc, Backends: cfg.Backends, Requesters: rs}
	if cs != nil {
		opts.Claude = func(ctx context.Context, user, backendID string) bool {
			a, err := cs.ClaudeAccounts(ctx, user, backendID, false)
			if err != nil {
				return !errors.Is(err, hub.ErrNoClaude)
			}
			return slices.ContainsFunc(a.Accounts, func(x hub.ClaudeAccount) bool { return x.ID != claude.ChatGPTAccount })
		}
	}
	return models.New(opts)
}

// newAgent returns the core of Settings > Agent, Skills and tools,
// Diagnostics and the chat's quick panel, or nil when there is no backend.
func newAgent(cfg *config.Config, svc *service.Service, dialers map[string]*gateway.Dialer) *agentcfg.Service {
	if len(cfg.Backends) == 0 {
		return nil
	}
	rs := make(map[string]device.Requester, len(dialers))
	for id, d := range dialers {
		rs[id] = d
	}
	return agentcfg.New(agentcfg.Options{Access: svc, Requesters: rs, Locked: cfg.Agent.LockedKeys})
}

// updateSources tell the update check what this install runs: each
// backend's agent release (from its device plugin) and paired computers.
func updateSources(cfg *config.Config, st *store.Store, devices *device.Server) updates.Sources {
	src := updates.Sources{Computers: func(ctx context.Context) [][2]string {
		v, err := st.ClientVersions(ctx)
		if err != nil {
			return nil
		}
		return v
	}}
	if devices == nil {
		return src
	}
	src.Agents = func(ctx context.Context) map[string]string {
		out := map[string]string{}
		for _, b := range cfg.Backends {
			p := devices.Plugin(b.ID)
			if p == nil {
				continue
			}
			if info, err := p.Info(ctx); err == nil && info.HermesVersion != "" {
				out[b.Name] = info.HermesVersion
			}
		}
		return out
	}
	return src
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

// tryLock is the store's advisory lock as a leader.TryLock.
func tryLock(st *store.Store, key int64) leader.TryLock {
	return func(ctx context.Context) (leader.Lease, bool, error) {
		l, held, err := st.TryLock(ctx, key, botCheckInterval)
		if !held {
			return nil, false, err
		}
		return l, true, nil
	}
}

// runBot runs the Telegram bot in the one replica that holds the bot's
// leader lock. With SQLite there is one replica, and it always leads.
func runBot(ctx context.Context, st *store.Store, bot *telegram.Bot, cfg config.Telegram, done chan<- struct{}) {
	defer close(done)
	log := slog.Default().With("component", "telegram")
	key := leader.Key("telegram/" + strconv.FormatInt(telegram.BotID(cfg.Token), 10))
	leader.Run(ctx, tryLock(st, key), cfg.LeaderRetry, log, func(ctx context.Context) {
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
