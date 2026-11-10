package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/allowlist"
	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/USA-RedDragon/hermes-device/internal/daemonstate"
	"github.com/USA-RedDragon/hermes-device/internal/device"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/internal/splice"
	"github.com/USA-RedDragon/hermes-device/internal/tokenstore"
	"github.com/spf13/cobra"
)

const whoamiTimeout = 15 * time.Second

var errSpliceStopped = errors.New("stopped unexpectedly")

func newRunCommand(b *base) *cobra.Command {
	var noSplice bool
	cmd := &cobra.Command{
		Use:   "run",
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
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			return runDaemon(ctx, cfg, b.version, b.userAgent(), newSplice)
		},
	}
	cmd.Flags().BoolVar(&noSplice, "no-splice", false, "Run only the exec leg, without the chat splice (sets gateway.splice to false)")
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
}

func newSplice(opts splice.Options) spliceServer { return splice.New(opts) }

// runDaemon runs the exec leg and the splice until ctx ends or the exec leg
// stops for good.
func runDaemon(ctx context.Context, cfg *config.Config, version, userAgent string, mkSplice func(splice.Options) spliceServer) error {
	if err := cfg.ForRun(); err != nil {
		return err
	}
	token, deviceID, stateDir, err := runToken(cfg)
	if err != nil {
		return err
	}
	hub := hubapi.New(cfg.Hub.URL, token, userAgent)

	allow, err := allowlist.New(cfg.Device.Roots)
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
	dev, err := device.New(device.Options{
		Hub: hub, Allow: allow, Shell: cfg.Device.Shell, TempDir: tempDir, ClientVersion: version, Env: commandEnv(os.Environ()),
		MinBackoff: cfg.Reconnect.MinBackoff, MaxBackoff: cfg.Reconnect.MaxBackoff, PingInterval: cfg.PingInterval,
	})
	if err != nil {
		return err
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
	srv := mkSplice(splice.Options{Hub: hub, PingInterval: cfg.PingInterval})

	pid := os.Getpid()
	if err := daemonstate.Write(stateDir, daemonstate.State{
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
		<-spliceDone
		return err
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

// tokenEnv is the env var of hub.token, which commands must not inherit.
const tokenEnv = config.EnvPrefix + "HUB" + config.EnvSeparator + "TOKEN"

// commandEnv is environ without the device token.
func commandEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if !strings.HasPrefix(kv, tokenEnv+"=") {
			out = append(out, kv)
		}
	}
	return out
}
