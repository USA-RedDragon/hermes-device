package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/internal/browser/host"
	"github.com/USA-RedDragon/talaria/internal/config"
	"github.com/USA-RedDragon/talaria/internal/daemonstate"
	"github.com/USA-RedDragon/talaria/internal/hubapi"
	"github.com/USA-RedDragon/talaria/internal/pairing"
	"github.com/USA-RedDragon/talaria/internal/setup"
	"github.com/USA-RedDragon/talaria/internal/splice"
	"github.com/USA-RedDragon/talaria/internal/tokenstore"
	"github.com/spf13/cobra"
)

// ErrNoHub is returned when setup has no hub URL.
var ErrNoHub = errors.New("give the hub URL: talaria setup https://hub.example.com")

// ErrHubMismatch is returned when the hub URL argument and the config
// file's hub.url differ.
var ErrHubMismatch = errors.New("the config file names another hub")

type setupFlags struct {
	name                 string
	binDir               string
	force, overwrite     bool
	noService, noBrowser bool
	noShell, uninstall   bool
	wait                 time.Duration
	shell, wslDistro     string
}

func newSetupCommand(b *base) *cobra.Command {
	var f setupFlags
	c := &cobra.Command{
		Use:   "setup [hub-url]",
		Short: "Set this computer up: install the binary, pair, write the config, start the service and connect Firefox",
		Long: "setup installs talaria for this user in one step: it copies this binary to ~/.talaria/bin, " +
			"pairs the computer with the hub (as login does), writes the config file when there is none, registers " +
			"Firefox's native messaging host and opens the extension's install page, starts a systemd user service " +
			"(launchd agent on macOS), offers to put ~/.talaria/bin on PATH for your login shell and bash -l, and offers to fix " +
			"bash's PATH for zsh and fish users. On Windows it registers a scheduled task that starts talaria at logon, " +
			"puts talaria on your PATH and asks which shell the agent's commands run in (--shell). Run it again at any time; " +
			"it only changes what is missing. setup --uninstall reverses what setup did.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sys, err := b.system()
			if err != nil {
				return err
			}
			sys.Out = cmd.OutOrStdout()
			cfg, err := b.loadMaybeMissing(cmd)
			if err != nil {
				return err
			}
			if f.uninstall {
				return b.uninstall(cmd.Context(), sys, cfg)
			}
			arg := ""
			if len(args) == 1 {
				arg = args[0]
			}
			return b.setup(cmd, sys, cfg, &f, arg)
		},
	}
	hostname, _ := os.Hostname()
	fl := c.Flags()
	fl.StringVar(&f.name, "name", hostname, "Name to suggest for this computer on the hub")
	fl.StringVar(&f.binDir, "bin-dir", "", "Directory to install the binary in (default ~/"+setup.BinDir+")")
	fl.BoolVar(&f.force, "force", false, "Replace a newer installed binary, or a service file that setup did not write (after a backup)")
	fl.BoolVar(&f.overwrite, "overwrite-config", false, "Replace an existing config file (after a backup)")
	fl.BoolVar(&f.noService, "no-service", false, "Do not install or start the background service")
	fl.BoolVar(&f.noBrowser, "no-browser", false, "Leave Firefox alone and write browser.enabled false into a new config")
	fl.BoolVar(&f.noShell, "no-shell-check", false, "Do not check or change the PATH of the login shell and bash -l")
	fl.BoolVar(&f.uninstall, "uninstall", false, "Undo what setup did, as its state file records")
	fl.StringVar(&f.shell, "shell", "", "Windows: the shell for agent commands in a new config, git-bash, powershell or wsl (default: ask, or Git Bash when installed)")
	fl.StringVar(&f.wslDistro, "wsl-distro", "", "Windows: the WSL distribution for --shell wsl (default: WSL's default)")
	fl.DurationVar(&f.wait, "wait", 10*time.Minute, "How long to wait for the Firefox extension to connect; 0 skips")
	return c
}

// loadMaybeMissing loads the config like load, but a config file that -c
// names and that does not exist yet reads as empty: setup writes it.
func (b *base) loadMaybeMissing(cmd *cobra.Command) (*config.Config, error) {
	fl := cmd.Flag("config")
	if fl == nil || fl.Value.String() == "" {
		return b.load()
	}
	named := fl.Value.String()
	if _, err := os.Stat(named); !errors.Is(err, fs.ErrNotExist) {
		return b.load()
	}
	empty, err := os.CreateTemp("", "talaria-setup-*.yaml")
	if err != nil {
		return nil, err
	}
	defer os.Remove(empty.Name())
	_, err = empty.WriteString("{}\n")
	if cerr := empty.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if err := fl.Value.Set(empty.Name()); err != nil {
		return nil, err
	}
	defer func() { _ = fl.Value.Set(named) }()
	return b.load()
}

// configFile is the config file that -c names, else the default one.
func configFile(cmd *cobra.Command, sys *setup.System) (string, error) {
	if fl := cmd.Flag("config"); fl != nil && fl.Value.String() != "" {
		return filepath.Abs(fl.Value.String())
	}
	return filepath.Join(sys.ConfigDir, "talaria", "config.yaml"), nil
}

func (b *base) setup(cmd *cobra.Command, sys *setup.System, cfg *config.Config, f *setupFlags, arg string) error {
	ctx, stop := signalContext(cmd.Context())
	defer stop()
	cfgFile, err := configFile(cmd, sys)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(cfgFile)
	exists := statErr == nil

	hub := arg
	switch {
	case hub != "" && exists && cfg.Hub.URL != "" && strings.TrimRight(cfg.Hub.URL, "/") != strings.TrimRight(hub, "/") && !f.overwrite:
		return fmt.Errorf("%w: %s has hub.url %s, and you gave %s. Pass --overwrite-config to replace the file", ErrHubMismatch, cfgFile, cfg.Hub.URL, hub)
	case hub == "":
		hub = cfg.Hub.URL
	}
	if hub == "" {
		hub = b.hubDefault
	}
	if hub == "" {
		return ErrNoHub
	}
	if err := setup.CheckHubURL(hub); err != nil {
		return err
	}
	cfg.Hub.URL = hub

	st, stateDir, err := tokenStore(cfg)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	sock, err := browserSocket(cfg)
	if err != nil {
		return err
	}
	binDir := f.binDir
	if binDir != "" {
		if binDir, err = filepath.Abs(binDir); err != nil {
			return err
		}
	}
	out := cmd.OutOrStdout()
	o := &setup.Options{
		Version: b.version, Exe: exe, HubURL: hub, ConfigFile: cfgFile, ConfigExists: exists,
		BrowserEnabled: cfg.Browser.Enabled, StateDir: stateDir, BinDir: binDir, Force: f.force,
		OverwriteConfig: f.overwrite, NoService: f.noService, NoBrowser: f.noBrowser, NoShell: f.noShell, Wait: f.wait,
		Releases: b.releases(), Shell: f.shell, WSLDistro: f.wslDistro, Shells: availableShells(ctx),
		Pair: func(ctx context.Context) (bool, error) {
			return b.pairIfNeeded(ctx, out, sys, hub, st, f.name)
		},
		InstallHost:  func(bin string) (string, error) { return installHostManifest(bin, sys.Home, sock) },
		HostManifest: filepath.Join(host.ManifestDir(sys.Home), host.Name+".json"),
		BrowserStatus: func(ctx context.Context) (*setup.BrowserStatus, error) {
			return daemonBrowserStatus(ctx, stateDir)
		},
	}
	o.Releases.UserAgent = b.userAgent()
	rep, err := sys.Setup(ctx, o)
	rep.Print(sys, err)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "Run talaria setup again at any time; it only changes what is missing.")
	return err
}

// pairIfNeeded pairs unless the store holds a token that the hub accepts.
func (b *base) pairIfNeeded(ctx context.Context, out io.Writer, sys *setup.System, hub string, st *tokenstore.Store, name string) (bool, error) {
	rec, err := st.Load(hub)
	if err == nil {
		wctx, cancel := context.WithTimeout(ctx, whoamiTimeout)
		who, werr := hubapi.New(hub, rec.Token, b.userAgent()).Whoami(wctx)
		cancel()
		switch {
		case werr == nil:
			_, _ = fmt.Fprintf(out, "Paired as %s (%s).\n", who.DeviceName, who.DeviceID)
			return false, nil
		case !errors.Is(werr, hubapi.ErrUnauthorized):
			slog.Warn("Could not check the stored device token with the hub; keeping it", "error", werr)
			return false, nil
		}
		_, _ = fmt.Fprintln(out, "The hub no longer accepts the stored device token; pairing again.")
	} else if !errors.Is(err, tokenstore.ErrNoToken) {
		return false, err
	}
	p := &pairing.Pairer{Hub: hubapi.New(hub, "", b.userAgent()), Prompt: func(s *hubapi.PairStart) { openURL(sys, s.VerificationURI) }}
	if err := login(ctx, out, hub, p, st, name, b.version); err != nil {
		return false, err
	}
	return true, nil
}

// openURL opens u in the default browser when there is a desktop in front
// of the person who runs setup (not on a Mac over SSH).
func openURL(sys *setup.System, u string) {
	if !sys.HasDesktop() {
		return
	}
	prog := "xdg-open"
	if sys.GOOS == goosDarwin {
		prog = "open"
	}
	if p, err := sys.LookPath(prog); err == nil {
		_ = sys.Run.Start(p, u)
	}
}

func daemonControl(ctx context.Context, stateDir, method, path string) (*http.Response, error) {
	ds, err := daemonstate.Read(stateDir)
	if err != nil {
		return nil, err
	}
	if ds.Splice == "" || ds.ControlSecret == "" {
		return nil, daemonstate.ErrNotRunning
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+ds.Splice+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+ds.ControlSecret)
	return (&http.Client{Timeout: 10 * time.Second}).Do(req)
}

// daemonBrowserStatus asks the running daemon about the Firefox extension.
func daemonBrowserStatus(ctx context.Context, stateDir string) (*setup.BrowserStatus, error) {
	resp, err := daemonControl(ctx, stateDir, http.MethodGet, splice.PathControlBrowser)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s (is the daemon older than this binary?)", splice.PathControlBrowser, resp.Status)
	}
	var st splice.BrowserStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&st); err != nil {
		return nil, err
	}
	return &setup.BrowserStatus{Enabled: st.Enabled, Connected: st.Connected}, nil
}

func (b *base) uninstall(ctx context.Context, sys *setup.System, cfg *config.Config) error {
	st, stateDir, err := tokenStore(cfg)
	if err != nil {
		return err
	}
	rep, err := sys.Uninstall(ctx, &setup.UninstallOptions{
		StateDir:   stateDir,
		RemoveHost: func() error { return removeHostManifest(sys.Home) },
		Unpair:     st.Delete,
	})
	rep.Print(sys, err)
	return err
}
