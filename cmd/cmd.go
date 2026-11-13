// Package cmd wires the talaria command line.
package cmd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/USA-RedDragon/configulator/v2"
	"github.com/USA-RedDragon/talaria/internal/autoupdate"
	"github.com/USA-RedDragon/talaria/internal/config"
	"github.com/USA-RedDragon/talaria/internal/legacy"
	"github.com/USA-RedDragon/talaria/internal/setup"
	"github.com/USA-RedDragon/talaria/internal/userdir"
	"github.com/spf13/cobra"
)

// Option changes how NewCommand builds the command.
type Option func(*base)

// WithDefaultHub sets the hub URL that setup uses when neither the command
// line nor the config gives one. The release sets it from the build.
func WithDefaultHub(url string) Option {
	return func(b *base) { b.hubDefault = url }
}

// NewCommand returns the root command.
func NewCommand(version, commit string, opts ...Option) *cobra.Command {
	root := &cobra.Command{
		Use:   "talaria",
		Short: "Run a server agent's shell and file operations on this device",
		Long: "talaria sets this computer up in one step (setup), pairs it with the Talaria hub (login), runs the agent's commands here (run), " +
			"opens the agent's terminal chat against the server (chat) and bridges editors over ACP (acp). " +
			"On the server it installs the device plugin (plugin install) and hosts ACP processes (acp-host).",
		Version:       fmt.Sprintf("%s - %s", version, commit),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	b := &base{
		version: version,
		commit:  commit,
		loader:  config.NewLoader(root.PersistentFlags(), defaultConfigFiles()),
		system:  func() (*setup.System, error) { return setup.LocalSystem(runtime.GOOS) },
		releases: func() *setup.Releases {
			base := setup.DefaultReleasesURL
			if u := os.Getenv(setup.ReleasesURLEnv); u != "" {
				base = strings.TrimSuffix(u, "/")
			}
			return &setup.Releases{BaseURL: base, Client: &http.Client{Timeout: 10 * time.Minute}}
		},
		releaseLookup: defaultReleaseLookup,
		executable:    os.Executable,
		underService:  underService,
		exit:          os.Exit,
		getenv:        config.Getenv,
	}
	b.verifier = b.attestations
	for _, o := range opts {
		o(b)
	}
	root.PersistentPreRunE = func(c *cobra.Command, _ []string) error {
		b.migrateOnStart(c)
		return nil
	}
	root.AddCommand(
		b.interactive(newLoginCommand(b)),
		newRunCommand(b),
		b.interactive(newChatCommand(b)),
		b.interactive(newACPCommand(b)),
		newACPHostCommand(b),
		newPluginCommand(b),
		newCopySelfCommand(b),
		browserCommand(b),
		b.interactive(newSetupCommand(b)),
		newUpdateCommand(b),
	)
	return root
}

// base is what every subcommand shares.
type base struct {
	version string
	commit  string
	loader  *configulator.Configulator[config.Config]
	// hubDefault is the build's default hub URL for setup.
	hubDefault string
	// system and releases are the computer and the release site setup and
	// update work on. Tests replace them.
	system   func() (*setup.System, error)
	releases func() *setup.Releases
	// verifier checks release attestations; releaseLookup finds the
	// latest release with the shared cache.
	verifier      func() setup.AttestationVerifier
	releaseLookup func(userAgent string) *autoupdate.Releases
	// executable, underService and exit are what run's auto-update needs
	// of the process. Tests replace them.
	executable   func() (string, error)
	underService func() bool
	exit         func(code int)
	// runArgs are the arguments of the run daemon (from "run" on).
	runArgs []string
	// migrate moves the files of a release before the rename on start.
	migrate bool
	getenv  func(string) string
	// notice is the update notice of an interactive command, which
	// writes to noticeOut.
	notice    *autoupdate.Notice
	noticeOut io.Writer
	noticeCtx context.Context //nolint:containedctx // the command's, for the notice's background lookup
}

// load reads the configuration and sets up logging to stderr. stdout stays
// free for subcommands that speak a protocol on it (acp).
func (b *base) load() (*config.Config, error) {
	cfg, err := b.loader.Load()
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return nil, err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	b.startNotice(cfg)
	return cfg, nil
}

func (b *base) userAgent() string {
	return "talaria/" + b.version
}

// defaultConfigFiles is only the user config file: talaria runs from
// any directory, and a config.yaml of another tool there must not win.
func defaultConfigFiles() []string {
	dir, err := userdir.Config()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(legacy.Dir(filepath.Join(dir, "talaria")), "config.yaml")}
}

// browserCommand is the browser command, whose subcommands a person
// runs. Those that load the config show the update notice.
func browserCommand(b *base) *cobra.Command {
	c := newBrowserCommand(b)
	for _, sub := range c.Commands() {
		b.interactive(sub)
	}
	return c
}
