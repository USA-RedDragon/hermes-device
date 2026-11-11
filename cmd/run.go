package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/allowlist"
	"github.com/USA-RedDragon/hermes-device/internal/autoupdate"
	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
	"github.com/USA-RedDragon/hermes-device/internal/browser/host"
	"github.com/USA-RedDragon/hermes-device/internal/browser/launch"
	"github.com/USA-RedDragon/hermes-device/internal/claudeusage"
	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/USA-RedDragon/hermes-device/internal/daemonstate"
	"github.com/USA-RedDragon/hermes-device/internal/device"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/internal/mcp"
	"github.com/USA-RedDragon/hermes-device/internal/splice"
	"github.com/USA-RedDragon/hermes-device/internal/tokenstore"
	"github.com/spf13/cobra"
)

const whoamiTimeout = 15 * time.Second

var errSpliceStopped = errors.New("stopped unexpectedly")

// CheckConfigFlag makes run only check its config.
const CheckConfigFlag = "check-config"

const runCommand = "run"

func newRunCommand(b *base) *cobra.Command {
	var noSplice, checkConfig bool
	cmd := &cobra.Command{
		Use:   runCommand,
		Short: "Run the device daemon: the exec leg and the chat splice",
		Long: "Run the device daemon: the exec leg and the chat splice. With --no-splice (or gateway.splice false) " +
			"it runs only the exec leg, for a headless service device such as the sandbox pod.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := b.load()
			if err != nil {
				return err
			}
			if noSplice {
				cfg.Gateway.Splice = false
			}
			b.runArgs = runArgs(os.Args)
			if checkConfig {
				if err := cfg.ForRun(); err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "The config is valid for run.")
				return err
			}
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			err = runDaemon(ctx, cfg, b.version, b.userAgent(), newSplice, b.daemonUpdater(cfg))
			var restart *restartError
			if errors.As(err, &restart) {
				slog.Info("Exiting so that the service starts the new release", "new", restart.tag)
				stop()
				b.exit(exitRestart)
				return nil
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&noSplice, "no-splice", false, "Run only the exec leg, without the chat splice (sets gateway.splice to false)")
	cmd.Flags().BoolVar(&checkConfig, CheckConfigFlag, false, "Load and check the config the way run does, then exit; auto-update runs a new release with it before it installs it")
	return cmd
}

// ErrTokenFileMode is returned for a hub.token-file that others can read.
var ErrTokenFileMode = errors.New("hub.token-file is readable by others; make it mode 0600, 0400 or 0440")

// ErrEmptyToken is returned for a hub.token or hub.token-file without a token.
var ErrEmptyToken = errors.New("no device token in hub.token or hub.token-file")

// runToken returns the device token for run: hub.token, else hub.token-file,
// else the token store. deviceID is the stored device id, "" for the first two.
func runToken(cfg *config.Config) (token, deviceID, stateDir string, err error) {
	switch {
	case cfg.Hub.Token != "":
		token := strings.TrimSpace(cfg.Hub.Token)
		if token == "" {
			return "", "", "", ErrEmptyToken
		}
		return token, "", "", nil
	case cfg.Hub.TokenFile != "":
		fi, err := os.Stat(cfg.Hub.TokenFile)
		if err != nil {
			return "", "", "", err
		}
		if fi.Mode().Perm()&0o007 != 0 {
			return "", "", "", fmt.Errorf("%s (mode %o): %w", cfg.Hub.TokenFile, fi.Mode().Perm(), ErrTokenFileMode)
		}
		data, err := os.ReadFile(cfg.Hub.TokenFile)
		if err != nil {
			return "", "", "", err
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", "", "", fmt.Errorf("%s: %w", cfg.Hub.TokenFile, ErrEmptyToken)
		}
		return token, "", "", nil
	}
	st, dir, err := tokenStore(cfg)
	if err != nil {
		return "", "", "", err
	}
	rec, err := st.Load(cfg.Hub.URL)
	if err != nil {
		return "", "", "", err
	}
	return rec.Token, rec.DeviceID, dir, nil
}

// spliceServer is what the daemon needs of *splice.Server.
type spliceServer interface {
	Serve(ctx context.Context, ln net.Listener) error
	ControlSecret() string
	TUIs() int
}

func newSplice(opts splice.Options) spliceServer { return splice.New(opts) }

// runDaemon runs the exec leg and the splice until ctx ends or the exec leg
// stops for good. With au it also installs new releases; after an install
// it waits until the device is idle and returns *restartError.
func runDaemon(ctx context.Context, cfg *config.Config, version, userAgent string, mkSplice func(splice.Options) spliceServer,
	au *autoupdate.Daemon,
) (err error) {
	if err := cfg.ForRun(); err != nil {
		return err
	}
	token, deviceID, stateDir, err := runToken(cfg)
	if err != nil {
		return err
	}
	hub := hubapi.New(cfg.Hub.URL, token, userAgent)

	roots, err := cfg.Roots(os.UserHomeDir)
	if err != nil {
		return err
	}
	allow, err := allowlist.New(roots)
	if err != nil {
		return err
	}
	tempDir := cfg.Device.TempDir
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return fmt.Errorf("create device.tmp-dir %s: %w", tempDir, err)
	}
	opts := device.Options{
		Hub: hub, Allow: allow, Shell: cfg.Device.Shell, TempDir: tempDir, ClientVersion: version, Env: commandEnv(os.Environ()),
		MinBackoff: cfg.Reconnect.MinBackoff, MaxBackoff: cfg.Reconnect.MaxBackoff, PingInterval: cfg.PingInterval,
		ResumeBuffer: cfg.Device.ResumeBuffer, MaxGrace: cfg.Device.MaxGrace,
	}
	var br *broker.Broker
	if cfg.Browser.Enabled {
		if br, err = startBroker(ctx, cfg); err != nil {
			return err
		}
		opts.Browser = lockReporter{br}
		if cfg.Browser.StartFirefox {
			opts.StartBrowser = launch.New(cfg.Browser.FirefoxCommand).Launch
		}
	}
	if len(cfg.MCP.Servers) > 0 {
		mgr := newMCP(cfg, version)
		defer mgr.Close()
		opts.MCP = mgr
		slog.Info("MCP servers on", "servers", mgr.Names(), "idle_timeout", cfg.MCP.IdleTimeout)
	}
	server := newServerHermes()
	if cfg.Gateway.Splice {
		opts.OnWelcome = server.welcome
	}
	dev, err := device.New(opts)
	if err != nil {
		return err
	}
	defer startUsage(ctx, deviceUsage(cfg, stateDir), func(ctx context.Context, r claudeusage.Report) error { return hub.SendUsage(ctx, r) })()
	var live atomic.Value
	if au != nil {
		var stopUpdater func() string
		ctx, stopUpdater = startUpdater(ctx, au, dev, &live)
		defer func() {
			if tag := stopUpdater(); tag != "" && err == nil {
				err = &restartError{tag: tag}
			}
		}()
	}
	if !cfg.Gateway.Splice {
		slog.Info("Running the exec leg only (no chat splice)", "roots", allow.Roots())
		return dev.Run(ctx)
	}

	wctx, wcancel := context.WithTimeout(ctx, whoamiTimeout)
	who, err := hub.Whoami(wctx)
	wcancel()
	switch {
	case errors.Is(err, hubapi.ErrUnauthorized):
		return err
	case err != nil:
		slog.Warn("Could not ask the hub who this device is; starting anyway", "error", err)
	default:
		deviceID = who.DeviceID
		slog.Info("Device", "id", who.DeviceID, "name", who.DeviceName, "user", who.User, "backend", who.Backend,
			"hermes_version", who.HermesVersion, "plugin_version", who.PluginVersion)
	}
	if stateDir == "" {
		if stateDir = cfg.Store.StateDir; stateDir == "" {
			if stateDir, err = tokenstore.DefaultStateDir(); err != nil {
				return fmt.Errorf("find the state directory: %w", err)
			}
		}
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Gateway.Listen)
	if err != nil {
		return fmt.Errorf("listen on gateway.listen %s: %w", cfg.Gateway.Listen, err)
	}
	defer ln.Close()
	srv := mkSplice(splice.Options{Hub: hub, PingInterval: cfg.PingInterval, Browser: spliceBrowser(br)})
	live.Store(srv)

	pid := os.Getpid()
	if err := server.start(stateDir, daemonstate.State{
		PID: pid, Splice: ln.Addr().String(), ControlSecret: srv.ControlSecret(), DeviceID: deviceID, Hub: cfg.Hub.URL,
	}); err != nil {
		return fmt.Errorf("write the daemon state: %w", err)
	}
	defer func() {
		if err := daemonstate.Remove(stateDir, pid); err != nil {
			slog.Warn("Could not remove the daemon state file", "error", err)
		}
	}()
	slog.Info("Running", "splice", ln.Addr().String(), "state_dir", stateDir)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if builds := hermesBuilds(cfg, userAgent); builds != nil {
		fetchDone := make(chan struct{})
		go func() {
			defer close(fetchDone)
			server.fetchLoop(ctx, builds)
		}()
		defer func() {
			cancel()
			<-fetchDone
		}()
	}
	devDone := make(chan error, 1)
	spliceDone := make(chan error, 1)
	go func() { devDone <- dev.Run(ctx) }()
	go func() {
		err := srv.Serve(ctx, ln)
		_ = ln.Close()
		spliceDone <- err
	}()

	select {
	case err := <-devDone:
		cancel()
		return errors.Join(err, <-spliceDone)
	case err := <-spliceDone:
		stopped := ctx.Err() != nil
		cancel()
		devErr := <-devDone
		if stopped {
			return devErr
		}
		if err == nil {
			err = errSpliceStopped
		}
		return fmt.Errorf("splice: %w", err)
	}
}

// startUsage runs col with send until the returned stop is called. A nil
// col runs nothing.
func startUsage(ctx context.Context, col *claudeusage.Collector, send claudeusage.Sender) (stop func()) {
	if col == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		col.Run(ctx, send)
	}()
	return func() {
		cancel()
		<-done
	}
}

// deviceUsage returns the collector of this computer's Claude Code usage,
// or nil when usage.enabled is false, or unset in a container.
func deviceUsage(cfg *config.Config, stateDir string) *claudeusage.Collector {
	on := !claudeusage.InContainer(os.Getenv)
	if cfg.Usage.Enabled != nil {
		on = *cfg.Usage.Enabled
	}
	if !on {
		return nil
	}
	state := cfg.Usage.StateFile
	if state == "" {
		dir := stateDir
		if dir == "" {
			dir = cfg.Store.StateDir
		}
		if dir == "" {
			d, err := tokenstore.DefaultStateDir()
			if err != nil {
				slog.Warn("No state directory; Claude usage is counted again after each restart", "error", err)
			}
			dir = d
		}
		if dir != "" {
			state = filepath.Join(dir, "claude-usage.gob")
		}
	}
	home, _ := os.UserHomeDir()
	sources := claudeusage.DefaultSources(home, os.Getenv, cfg.Usage.ClaudeDirs)
	slog.Info("Counting Claude Code usage", "sources", len(sources))
	return claudeusage.New(claudeusage.Options{Sources: sources, StateFile: state})
}

// startUpdater runs au beside the daemon. The device is idle for au when
// it holds no work and live holds no splice with an open TUI. It returns
// the context for the daemon, which ends when au wants a restart, and a
// function that stops au and returns the tag to restart into, or "".
func startUpdater(ctx context.Context, au *autoupdate.Daemon, dev *device.Client, live *atomic.Value) (context.Context, func() string) {
	ctx, cancel := context.WithCancel(ctx)
	au.Drain = func() bool {
		if s, ok := live.Load().(spliceServer); ok && s.TUIs() > 0 {
			return false
		}
		return dev.Drain()
	}
	var tag string
	done := make(chan struct{})
	go func() {
		defer close(done)
		if tag = au.Run(ctx); tag != "" {
			cancel()
		}
	}()
	return ctx, func() string {
		cancel()
		<-done
		return tag
	}
}

// startBroker listens on the browser socket and serves it until ctx ends.
func startBroker(ctx context.Context, cfg *config.Config) (*broker.Broker, error) {
	sock, err := browserSocket(cfg)
	if err != nil {
		return nil, err
	}
	br, err := broker.Listen(broker.Options{Path: sock, ExtensionID: host.ExtensionID, AllowHosts: cfg.Browser.AllowHosts,
		ConfirmHosts: cfg.Browser.ConfirmHosts, AutologinHosts: cfg.Browser.AutologinHosts, HubURL: cfg.Hub.URL})
	if err != nil {
		return nil, fmt.Errorf("browser socket: %w", err)
	}
	go func() {
		if err := br.Serve(ctx); err != nil {
			slog.Error("Browser socket stopped", "error", err)
		}
	}()
	slog.Info("Browser control on", "socket", sock, "allow_hosts", cfg.Browser.AllowHosts,
		"autologin_hosts", cfg.Browser.AutologinHosts)
	return br, nil
}

// newMCP returns the manager of the configured MCP servers. They get the
// commands' environment, so not the device token either.
func newMCP(cfg *config.Config, version string) *mcp.Manager {
	servers := make(map[string]mcp.ServerConfig, len(cfg.MCP.Servers))
	for name, s := range cfg.MCP.Servers {
		servers[name] = mcp.ServerConfig{Command: s.Command, Args: s.Args, Env: s.Env, Dir: s.Dir, IdleTimeout: s.IdleTimeout}
	}
	return mcp.New(mcp.Options{Servers: servers, Env: commandEnv(os.Environ()), IdleTimeout: cfg.MCP.IdleTimeout,
		ClientVersion: version})
}

// tokenEnv is the env var of hub.token, which commands must not inherit,
// and legacyTokenEnv its name before the rename.
const (
	tokenEnv       = config.EnvPrefix + "HUB" + config.EnvSeparator + "TOKEN"
	legacyTokenEnv = config.LegacyEnvPrefix + "HUB" + config.EnvSeparator + "TOKEN"
)

// commandEnv is environ without the device token.
func commandEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, tokenEnv+"=") && !strings.HasPrefix(kv, legacyTokenEnv+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// runArgs are the arguments of this run daemon, which auto-update runs a
// new release with to check that it accepts this config.
func runArgs(args []string) []string {
	if len(args) > 1 && args[1] == runCommand {
		return append([]string{}, args[1:]...)
	}
	return []string{runCommand}
}
