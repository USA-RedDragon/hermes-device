package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/internal/pairing"
	"github.com/USA-RedDragon/hermes-device/internal/tokenstore"
	"github.com/spf13/cobra"
)

func newLoginCommand(b *base) *cobra.Command {
	var name string
	c := &cobra.Command{
		Use:   "login",
		Short: "Pair this device with the Talaria hub and store its token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := b.load()
			if err != nil {
				return err
			}
			if err := cfg.ForHub(); err != nil {
				return err
			}
			st, _, err := tokenStore(cfg)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			p := &pairing.Pairer{Hub: hubapi.New(cfg.Hub.URL, "", b.userAgent())}
			return login(ctx, cmd.OutOrStdout(), cfg.Hub.URL, p, st, name, b.version)
		},
	}
	hostname, _ := os.Hostname()
	c.Flags().StringVar(&name, "name", hostname, "Name to suggest for this device on the hub")
	return c
}

// login pairs, stores the token and tells the user where it went.
func login(ctx context.Context, out io.Writer, hub string, p *pairing.Pairer, st *tokenstore.Store, name, version string) error {
	hostname, _ := os.Hostname()
	if name == "" {
		name = hostname
	}
	then := p.Prompt
	p.Prompt = func(s *hubapi.PairStart) {
		_, _ = fmt.Fprintf(out, "Open %s\nsign in, enter the code %s and confirm this device.\n", s.VerificationURI, s.UserCode)
		if then != nil {
			then(s)
		}
	}
	res, err := p.Pair(ctx, hubapi.PairRequest{
		Name: name, Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH, ClientVersion: version,
	})
	if err != nil {
		return err
	}
	where, err := st.Save(tokenstore.Record{
		Hub: hub, Token: res.Token, DeviceID: res.DeviceID, DeviceName: res.DeviceName, Backend: res.Backend,
	})
	if err != nil {
		return fmt.Errorf("store the device token: %w", err)
	}
	if where == "keyring" {
		where = "the OS keyring (service " + tokenstore.Service + ")"
	}
	_, _ = fmt.Fprintf(out, "Paired as %s (%s) with backend %s. Token stored in %s.\n", res.DeviceName, res.DeviceID, res.Backend, where)
	return nil
}

// tokenStore returns the token store and the state directory cfg names.
func tokenStore(cfg *config.Config) (*tokenstore.Store, string, error) {
	dir := cfg.Store.StateDir
	if dir == "" {
		d, err := tokenstore.DefaultStateDir()
		if err != nil {
			return nil, "", fmt.Errorf("find the state directory: %w", err)
		}
		dir = d
	}
	file := cfg.Store.TokenFile
	if file == "" {
		file = filepath.Join(dir, "token")
	}
	return tokenstore.New(cfg.Store.Keyring, file), dir, nil
}
