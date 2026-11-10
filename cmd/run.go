package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/allowlist"
	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/USA-RedDragon/hermes-device/internal/daemonstate"
	"github.com/USA-RedDragon/hermes-device/internal/device"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/internal/splice"
	"github.com/spf13/cobra"
)

const whoamiTimeout = 15 * time.Second

var errSpliceStopped = errors.New("stopped unexpectedly")

func newRunCommand(b *base) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the device daemon: the exec leg and the chat splice",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := b.load()
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			return runDaemon(ctx, cfg, b.version, b.userAgent(), newSplice)
		},
	}
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
	st, stateDir, err := tokenStore(cfg)
	if err != nil {
		return err
	}
	rec, err := st.Load(cfg.Hub.URL)
	if err != nil {
		return err
	}
	hub := hubapi.New(cfg.Hub.URL, rec.Token, userAgent)

	deviceID := rec.DeviceID
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

	allow, err := allowlist.New(cfg.Device.Roots)
	if err != nil {
		return err
	}
	dev, err := device.New(device.Options{
		Hub: hub, Allow: allow, Shell: cfg.Device.Shell, TempDir: cfg.Device.TempDir, ClientVersion: version,
		MinBackoff: cfg.Reconnect.MinBackoff, MaxBackoff: cfg.Reconnect.MaxBackoff, PingInterval: cfg.PingInterval,
	})
	if err != nil {
		return err
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
