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
	"github.com/USA-RedDragon/hermes-device/internal/hermesbuild"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/internal/oci"
	"github.com/USA-RedDragon/hermes-device/internal/splice"
	"github.com/USA-RedDragon/hermes-device/internal/tokenstore"
)

// GatewayURLEnv is the variable the Ink TUI dials instead of starting its
// own gateway (ui-tui/src/gatewayClient.ts).
const GatewayURLEnv = "HERMES_TUI_GATEWAY_URL"

const (
	versionTimeout = 60 * time.Second
	controlTimeout = 10 * time.Second
	// lookupTimeout is above the daemon's own bound on a lookup.
	lookupTimeout = 3 * time.Minute
)

// Errors from chat.
var (
	ErrNotRunning      = errors.New("talaria run is not running; start it first")
	ErrOtherHub        = errors.New("the running talaria run uses another hub")
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

func (c *Control) do(ctx context.Context, method, path string, body any, timeout time.Duration) (*http.Response, error) {
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
		client = &http.Client{Timeout: timeout}
	}
	resp, err := client.Do(req)
	var ue *url.Error
	if errors.As(err, &ue) && ue.Timeout() {
		return nil, fmt.Errorf("the talaria run daemon did not answer in time: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotRunning, err)
	}
	return resp, nil
}

// NewToken asks the daemon for a per-run secret tied to dir, the absolute
// device directory chat runs in.
func (c *Control) NewToken(ctx context.Context, dir string) (string, error) {
	resp, err := c.do(ctx, http.MethodPost, splice.PathControlTokens, map[string]string{"cwd": dir}, controlTimeout)
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
	resp, err := c.do(ctx, http.MethodDelete, splice.PathControlTokens+"/"+url.PathEscape(token), nil, controlTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("daemon refused to delete the chat token: %s", resp.Status)
	}
	return nil
}

// Resume flags of Hermes, as resumeFlag names them.
const (
	flagContinue = "continue"
	flagResume   = "resume"
)

// ErrNoSession is a resume whose session the server does not have.
var ErrNoSession = errors.New("no session to resume")

// Lookup asks the daemon for the id of the session name names (a session
// id or a title), or of the most recent session bound to this device when
// name is empty.
func (c *Control) Lookup(ctx context.Context, name string) (string, error) {
	body := map[string]any{"latest": true}
	if name != "" {
		body = map[string]any{"session": name}
	}
	resp, err := c.do(ctx, http.MethodPost, splice.PathControlLookup, body, lookupTimeout)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		SessionID string `json:"session_id"`
		Error     string `json:"error"`
		Message   string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
	switch {
	case resp.StatusCode == http.StatusNotFound && name == "":
		return "", fmt.Errorf("%w: no session on the server is bound to this device", ErrNoSession)
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("%w: no session on the server matches %q", ErrNoSession, name)
	case resp.StatusCode != http.StatusOK || out.SessionID == "":
		return "", fmt.Errorf("daemon could not look up the session: %s %s", resp.Status, out.Message)
	}
	return out.SessionID, nil
}

// ResumeTarget finds Hermes' resume flags in args: -c/--continue with an
// optional name and -r/--resume with a session id, a title or latest, in
// the forms argparse reads them, up to a "--". The local Hermes would look
// them up in its own local session store, which does not hold the
// server's sessions. It returns args without those flags, the name to look
// up ("" for the latest session) and whether args resume at all. --resume
// wins over --continue, and an empty value resumes nothing, as in Hermes.
// A --resume with no value, or with one that starts with "-", stays in
// rest for Hermes to report.
func ResumeTarget(args []string) (rest []string, name string, resume bool) {
	var res *string
	var cont, bare bool
	var contName string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		flag, value, attached := resumeFlag(a)
		switch flag {
		case "":
			rest = append(rest, a)
		case flagContinue:
			// The name is optional: argparse takes the next argument
			// unless it starts with "-".
			cont, bare, contName = true, false, value
			switch {
			case attached:
			case i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
				i++
				contName = args[i]
			default:
				bare = true
			}
		case flagResume:
			switch {
			case attached:
				res = &value
			case i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
				i++
				res = &args[i]
			default:
				rest = append(rest, a)
			}
		}
	}
	switch {
	case res != nil && *res != "":
		if strings.EqualFold(strings.TrimSpace(*res), "latest") {
			return rest, "", true
		}
		return rest, *res, true
	case cont && bare:
		return rest, "", true
	case cont && contName != "":
		return rest, contName, true
	}
	return rest, "", false
}

// resumeFlag names the resume flag a is (flagContinue, flagResume or ""),
// with the value attached to it (-cNAME, -c=NAME, --continue=NAME).
// argparse takes a unique prefix of a long flag: among Hermes' flags,
// --co is the shortest for --continue and --res for --resume.
func resumeFlag(a string) (flag, value string, attached bool) {
	if long, ok := strings.CutPrefix(a, "--"); ok {
		name, value, attached := strings.Cut(long, "=")
		switch {
		case len(name) >= 2 && strings.HasPrefix(flagContinue, name):
			return flagContinue, value, attached
		case len(name) >= 3 && strings.HasPrefix(flagResume, name):
			return flagResume, value, attached
		}
		return "", "", false
	}
	if len(a) >= 2 && a[0] == '-' {
		value, attached := strings.CutPrefix(a[2:], "=")
		attached = attached || value != ""
		switch a[1] {
		case 'c':
			return flagContinue, value, attached
		case 'r':
			return flagResume, value, attached
		}
	}
	return "", "", false
}

// resolveArgs replaces the resume flags in args with --resume and the id
// of the server's session they name.
func resolveArgs(ctx context.Context, c *Control, args []string) ([]string, error) {
	rest, name, resume := ResumeTarget(args)
	if !resume {
		return args, nil
	}
	sid, err := c.Lookup(ctx, name)
	if errors.Is(err, ErrNoSession) && name != "" {
		// Not a session the lookup finds, such as a messaging session
		// moved to this device: the splice resumes the name as sent.
		sid, err = name, nil
	}
	if err != nil {
		return nil, err
	}
	return append([]string{"--resume", sid}, rest...), nil
}

// Params configure Run.
type Params struct {
	// Hermes is the local Hermes launcher, used when Builds is nil or
	// Image is empty.
	Hermes string
	// Builds holds the managed Hermes builds; nil turns them off.
	Builds *hermesbuild.Store
	// Image is the server's Hermes image from the daemon state, and
	// BaseImage the upstream image it builds on, which chat runs when
	// Image cannot be fetched.
	Image, BaseImage string
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

// launch is how runTUI starts the TUI.
type launch struct {
	argv []string
	// env replaces the base environment when set.
	env []string
	// keep is passed to the TUI and closed after it.
	keep *os.File
}

// Run picks the Hermes to run, starts its TUI against the splice and
// returns its exit code. A TUI killed by a signal gives 128 plus the
// signal number. With Builds and an Image pinned by digest it runs that
// exact build, fetching it first when needed; otherwise it runs Hermes
// after checking that its version is the server's.
func Run(ctx context.Context, p Params) (int, error) {
	log := p.Log
	if log == nil {
		log = slog.Default()
	}
	l, err := pickHermes(ctx, p, log)
	if err != nil {
		return 0, err
	}
	if l.keep != nil {
		defer l.keep.Close()
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
	if p.Args, err = resolveArgs(ctx, p.Control, p.Args); err != nil {
		return 0, err
	}
	l.argv = append(l.argv, p.Args...)
	return runTUI(p, l, token)
}

// pickHermes returns the launch of the managed build of p.Image, or of
// p.Hermes once its version matches the server's.
func pickHermes(ctx context.Context, p Params, log *slog.Logger) (*launch, error) {
	base := p.Env
	if base == nil {
		base = os.Environ()
	}
	if p.Builds != nil && p.Image != "" {
		_, err := oci.ParseReference(p.Image)
		if err == nil {
			var l *launch
			l, err = managedLaunch(ctx, p.Builds, pickImage(p), base)
			if err != nil && !errors.Is(err, oci.ErrPlatform) && ctx.Err() == nil && p.BaseImage != "" && pickImage(p) == p.Image {
				log.Warn("Could not fetch the server's Hermes build; running the upstream build it is based on",
					"image", p.Image, "base", p.BaseImage, "error", err)
				l, err = managedLaunch(ctx, p.Builds, p.BaseImage, base)
			}
			if !errors.Is(err, oci.ErrPlatform) {
				return l, err
			}
		}
		log.Warn("Cannot run the server's Hermes build here; using chat.hermes", "image", p.Image, "error", err)
	}
	who, err := p.Hub.Whoami(ctx)
	if err != nil {
		return nil, fmt.Errorf("whoami: %w", err)
	}
	local, err := HermesVersion(ctx, p.Hermes)
	if err != nil {
		return nil, err
	}
	if !SameVersion(local, who.HermesVersion) {
		if !p.AllowVersionMismatch {
			return nil, fmt.Errorf("%w: local %s, server %s; install the server's release or pass --allow-version-mismatch",
				ErrVersionMismatch, local, who.HermesVersion)
		}
		log.Warn("local and server Hermes differ", "local", local, "server", who.HermesVersion)
	}
	return &launch{argv: Argv(p.Hermes, nil), env: base}, nil
}

// pickImage is p.Image, or p.BaseImage when only the base image's build
// is on this device: run fetches the base build when it cannot pull the
// server's image.
func pickImage(p Params) string {
	if p.BaseImage == "" {
		return p.Image
	}
	if _, err := p.Builds.Open(p.Image); errors.Is(err, hermesbuild.ErrNotPulled) {
		if _, err := oci.ParseReference(p.BaseImage); err == nil {
			if _, err := p.Builds.Open(p.BaseImage); err == nil {
				return p.BaseImage
			}
		}
	}
	return p.Image
}

// managedLaunch fetches image's build when needed and marks it in use.
func managedLaunch(ctx context.Context, builds *hermesbuild.Store, image string, base []string) (*launch, error) {
	for range 3 {
		b, err := builds.Ensure(ctx, image)
		if err != nil {
			return nil, err
		}
		keep, err := builds.Use(ctx, b)
		if errors.Is(err, hermesbuild.ErrNotPulled) {
			continue
		}
		if err != nil {
			return nil, err
		}
		launcher, err := b.Launcher()
		if err == nil {
			var env []string
			if env, err = builds.Env(b, base); err == nil {
				return &launch{argv: Argv(launcher, nil), env: env, keep: keep}, nil
			}
		}
		_ = keep.Close()
		return nil, err
	}
	return nil, fmt.Errorf("%w: %s was removed while chat started", hermesbuild.ErrNotPulled, image)
}

func runTUI(p Params, l *launch, token string) (int, error) {
	argv := l.argv
	//nolint:gosec,noctx // the user's configured Hermes or the verified server build; chat forwards signals to it and waits, so no context may kill it
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = Env(l.env, p.Control.State.Splice, token)
	cmd.Dir = p.Dir
	if l.keep != nil {
		cmd.ExtraFiles = []*os.File{l.keep}
	}
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
		return 0, fmt.Errorf("start %s: %w", argv[0], err)
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
