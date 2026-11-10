// Package chat runs the local Hermes TUI against the run daemon's splice
// (docs/protocol.md sections 5.2, 5.5 and 7.3).
package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/USA-RedDragon/hermes-device/internal/daemonstate"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/internal/splice"
	"github.com/USA-RedDragon/hermes-device/internal/tokenstore"
)

// GatewayURLEnv is the variable the Ink TUI dials instead of starting its
// own gateway (ui-tui/src/gatewayClient.ts).
const GatewayURLEnv = "HERMES_TUI_GATEWAY_URL"

const (
	versionTimeout = 60 * time.Second
	controlTimeout = 10 * time.Second
)

// Errors from chat.
var (
	ErrNotRunning      = errors.New("hermes-device run is not running; start it first")
	ErrOtherHub        = errors.New("the running hermes-device run uses another hub")
	ErrVersionMismatch = errors.New("local Hermes and server Hermes are different releases")
	ErrNoVersion       = errors.New("could not read the version from hermes --version")
)

// StateDir is store.state-dir, or its default.
func StateDir(cfg *config.Config) (string, error) {
	if cfg.Store.StateDir != "" {
		return cfg.Store.StateDir, nil
	}
	return tokenstore.DefaultStateDir()
}

// TokenFile is store.token-file, or token in stateDir.
func TokenFile(cfg *config.Config, stateDir string) string {
	if cfg.Store.TokenFile != "" {
		return cfg.Store.TokenFile
	}
	return filepath.Join(stateDir, "token")
}

// ReadState reads daemon.json in dir and checks that its daemon is alive
// and uses hub.
func ReadState(dir, hub string) (*daemonstate.State, error) {
	st, err := daemonstate.Read(dir)
	if errors.Is(err, daemonstate.ErrNotRunning) {
		return nil, ErrNotRunning
	}
	if err != nil {
		return nil, err
	}
	if st.PID <= 0 || errors.Is(syscall.Kill(st.PID, 0), syscall.ESRCH) {
		return nil, fmt.Errorf("%w (stale %s from pid %d)", ErrNotRunning, daemonstate.FileName, st.PID)
	}
	if st.Splice == "" || st.ControlSecret == "" {
		return nil, fmt.Errorf("%s: no splice address or control secret", daemonstate.FileName)
	}
	if hub != "" && strings.TrimRight(st.Hub, "/") != strings.TrimRight(hub, "/") {
		return nil, fmt.Errorf("%w: %s, not %s", ErrOtherHub, st.Hub, hub)
	}
	return st, nil
}

var versionRE = regexp.MustCompile(`\bv?(\d+\.\d+\.\d+\S*)`)

// ParseVersion returns the version in `hermes --version` output, whose
// first line reads "Hermes Agent v0.22.7 (2026.9.24)" (hermes_cli/banner.py
// format_banner_version_label).
func ParseVersion(out string) (string, error) {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	m := versionRE.FindStringSubmatch(first)
	if m == nil {
		return "", fmt.Errorf("%w: %q", ErrNoVersion, first)
	}
	return m[1], nil
}

// HermesVersion runs `<hermes> --version` and returns the version.
func HermesVersion(ctx context.Context, hermes string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, hermes, "--version")
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s --version: %w: %s", hermes, err, strings.TrimSpace(errOut.String()))
	}
	return ParseVersion(out.String())
}

// SameVersion reports whether two Hermes versions name the same release,
// ignoring a leading "v".
func SameVersion(local, server string) bool {
	return strings.TrimPrefix(local, "v") == strings.TrimPrefix(server, "v")
}

// GatewayURL is the URL the TUI dials on the splice.
func GatewayURL(spliceAddr, token string) string {
	return "ws://" + spliceAddr + splice.PathTUI + "?token=" + url.QueryEscape(token)
}

// Env is base with GatewayURLEnv set to the splice URL for token.
func Env(base []string, spliceAddr, token string) []string {
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if !strings.HasPrefix(kv, GatewayURLEnv+"=") {
			out = append(out, kv)
		}
	}
	return append(out, GatewayURLEnv+"="+GatewayURL(spliceAddr, token))
}

// Argv is the TUI launch: `<hermes> --tui <args>`. --tui forces the Ink
// TUI whatever display.interface says (hermes_cli/main_tui_launch.py
// _resolve_use_tui).
func Argv(hermes string, args []string) []string {
	return append([]string{hermes, "--tui"}, args...)
}

// Control calls the run daemon's control endpoints.
type Control struct {
	State *daemonstate.State
	HTTP  *http.Client
}

func (c *Control) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.State.Splice+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.State.ControlSecret)
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: controlTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotRunning, err)
	}
	return resp, nil
}

// NewToken asks the daemon for a per-run secret tied to dir, the absolute
// device directory chat runs in.
func (c *Control) NewToken(ctx context.Context, dir string) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, splice.PathControlTokens, map[string]string{"cwd": dir})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("daemon refused a chat token: %s", resp.Status)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil || out.Token == "" {
		return "", fmt.Errorf("daemon sent a bad token answer: %w", err)
	}
	return out.Token, nil
}

// DeleteToken revokes token and closes the sockets that use it.
func (c *Control) DeleteToken(ctx context.Context, token string) error {
	resp, err := c.do(ctx, http.MethodDelete, splice.PathControlTokens+"/"+url.PathEscape(token), nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("daemon refused to delete the chat token: %s", resp.Status)
	}
	return nil
}

// Params configure Run.
type Params struct {
	// Hermes is the local Hermes launcher.
	Hermes string
	// Args go to Hermes after --tui.
	Args []string
	Hub  *hubapi.Client
	// Control reaches the run daemon.
	Control *Control
	// AllowVersionMismatch starts the TUI even when the versions differ.
	AllowVersionMismatch bool
	Stdin                io.Reader
	Stdout, Stderr       io.Writer
	// Env is the environment the TUI starts from. Nil means os.Environ().
	Env []string
	// Dir is the absolute directory the TUI runs in. Empty means the
	// current one.
	Dir string
	// Signals, when set, replaces the process signal subscription (for
	// tests).
	Signals <-chan os.Signal
	Log     *slog.Logger
}

// Run checks versions, starts the TUI against the splice and returns its
// exit code. A TUI killed by a signal gives 128 plus the signal number.
func Run(ctx context.Context, p Params) (int, error) {
	log := p.Log
	if log == nil {
		log = slog.Default()
	}
	who, err := p.Hub.Whoami(ctx)
	if err != nil {
		return 0, fmt.Errorf("whoami: %w", err)
	}
	local, err := HermesVersion(ctx, p.Hermes)
	if err != nil {
		return 0, err
	}
	if !SameVersion(local, who.HermesVersion) {
		if !p.AllowVersionMismatch {
			return 0, fmt.Errorf("%w: local %s, server %s; install the server's release or pass --allow-version-mismatch",
				ErrVersionMismatch, local, who.HermesVersion)
		}
		log.Warn("local and server Hermes differ", "local", local, "server", who.HermesVersion)
	}
	dir := p.Dir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return 0, fmt.Errorf("current directory: %w", err)
		}
	}
	token, err := p.Control.NewToken(ctx, dir)
	if err != nil {
		return 0, err
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlTimeout)
		defer cancel()
		if err := p.Control.DeleteToken(dctx, token); err != nil {
			log.Warn("could not revoke the chat token", "error", err)
		}
	}()
	return runTUI(p, token)
}

func runTUI(p Params, token string) (int, error) {
	env := p.Env
	if env == nil {
		env = os.Environ()
	}
	argv := Argv(p.Hermes, p.Args)
	//nolint:gosec // the launcher is the user's own configured Hermes
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = Env(env, p.Control.State.Splice, token)
	cmd.Dir = p.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = p.Stdin, p.Stdout, p.Stderr

	sigs := p.Signals
	if sigs == nil {
		ch := make(chan os.Signal, 4)
		// SIGINT from the terminal reaches the TUI through the foreground
		// process group, so chat only keeps it from killing itself.
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(ch)
		sigs = ch
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", p.Hermes, err)
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case sig := <-sigs:
				if sig != syscall.SIGINT {
					_ = cmd.Process.Signal(sig)
				}
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}
