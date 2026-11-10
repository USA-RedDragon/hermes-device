// Package acphost runs in the Hermes pod. It accepts the hub's ACP leg on
// /acp, starts one `hermes acp` per connection and copies JSON-RPC between
// the WebSocket and the child's stdio, one message per line, with the
// rewrites of docs/protocol.md section 6.2.
package acphost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/USA-RedDragon/hermes-device/assertion"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

// Close codes acp-host sends besides protocol.CloseAssertionRefused.
const (
	CloseTryAgainLater = websocket.StatusCode(1013)
	CloseChildExited   = websocket.StatusInternalError
)

// maxMessage is the largest stdout line acp-host copies. maxInbound is the
// largest WebSocket message: hermes acp reads stdin with a 50 MiB line
// limit (acp/core.py) and stops reading on a longer line.
const (
	maxMessage = 64 << 20
	maxInbound = 48 << 20
)

// Options configure a Server.
type Options struct {
	Verifier  *assertion.Verifier
	BackendID string
	// Hermes is the launcher; each connection runs `<Hermes> acp`.
	Hermes       string
	Placeholder  string
	BindingsFile string
	MaxProcesses int
	PingInterval time.Duration
	// StopGrace is how long a child may run after its stdin closes before
	// it gets SIGTERM; KillGrace is how long after SIGTERM before SIGKILL.
	// They default to 5s each.
	StopGrace time.Duration
	KillGrace time.Duration
	// GatewayHealthURL, when set, must answer 2xx before a child starts:
	// hermes acp starts a gateway of its own when it finds none.
	GatewayHealthURL string
	// SweepInterval is how often the host looks for processes that left a
	// child's process group (a gateway that hermes acp started). It
	// defaults to 2s.
	SweepInterval time.Duration
	Logger        *slog.Logger
}

// Server hosts hermes acp processes for hub connections.
type Server struct {
	opts   Options
	slots  chan struct{}
	base   context.Context //nolint:containedctx // server lifetime: Close cancels it to end every connection
	cancel context.CancelFunc
	wg     sync.WaitGroup
	nextID atomic.Uint64
	health *http.Client
	// orphans is set once the host is a child subreaper. Every process
	// reparented to it is then an orphan of a hermes acp child.
	orphans atomic.Bool

	mu       sync.Mutex
	children map[int]bool
	killing  map[int]bool
}

// New returns a server for opts.
func New(opts Options) *Server {
	if opts.StopGrace <= 0 {
		opts.StopGrace = 5 * time.Second
	}
	if opts.KillGrace <= 0 {
		opts.KillGrace = 5 * time.Second
	}
	if opts.PingInterval <= 0 {
		opts.PingInterval = protocol.PingInterval
	}
	if opts.MaxProcesses < 1 {
		opts.MaxProcesses = 1
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = 2 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		opts: opts, slots: make(chan struct{}, opts.MaxProcesses), base: ctx, cancel: cancel,
		health: &http.Client{Timeout: 3 * time.Second}, children: map[int]bool{}, killing: map[int]bool{},
	}
	go s.sweepLoop()
	return s
}

// Handler serves GET /acp and GET /healthz.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+protocol.ACPHostPath, s.handleACP)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// Serve serves on ln until ctx ends, then closes every connection and
// waits for every child to exit.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if err := becomeSubreaper(); err != nil {
		s.opts.Logger.Warn("Could not become a child subreaper; orphans of hermes acp go to init", "error", err)
	} else {
		s.orphans.Store(true)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		s.Close()
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	s.Close()
	return nil
}

// Close ends every connection and waits until every child has exited.
func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
}

func (s *Server) handleACP(w http.ResponseWriter, r *http.Request) {
	s.wg.Add(1)
	defer s.wg.Done()
	log := s.opts.Logger.With("conn", s.nextID.Add(1), "remote", r.RemoteAddr)
	claims, verr := s.opts.Verifier.Verify(r.Header.Get(assertion.Header), assertion.Expect{
		Audience: s.opts.BackendID, Leg: assertion.LegACP, Method: r.Method, Path: r.URL.Path,
	})
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Warn("ACP upgrade failed", "error", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()
	if verr != nil {
		log.Warn("Refused the hub assertion", "error", verr)
		_ = conn.Close(protocol.CloseAssertionRefused, "assertion refused")
		return
	}
	log = log.With("device", claims.DeviceID)
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		log.Warn("Refused an ACP connection: process limit reached", "max", s.opts.MaxProcesses)
		_ = conn.Close(CloseTryAgainLater, "too many hermes acp processes")
		return
	}
	if !s.gatewayUp(r.Context()) {
		log.Warn("Refused an ACP connection: the Hermes gateway does not answer", "url", s.opts.GatewayHealthURL)
		_ = conn.Close(CloseTryAgainLater, "Hermes gateway not running")
		return
	}
	conn.SetReadLimit(maxInbound)
	c := &connection{
		srv: s, conn: conn, log: log,
		rw: &rewriter{
			placeholder: s.opts.Placeholder, bindingsFile: s.opts.BindingsFile,
			deviceID: claims.DeviceID, lists: map[string]listRequest{},
		},
	}
	c.serve() //nolint:contextcheck // a connection derives from the server's base context so Close ends it
}

func (s *Server) gatewayUp(ctx context.Context) bool {
	if s.opts.GatewayHealthURL == "" {
		return true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.opts.GatewayHealthURL, nil)
	if err != nil {
		return false
	}
	resp, err := s.health.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// connection is one WebSocket and its hermes acp child.
type connection struct {
	srv  *Server
	conn *websocket.Conn
	log  *slog.Logger

	mu sync.Mutex
	rw *rewriter
}

func (c *connection) serve() {
	ctx, cancel := context.WithCancel(c.srv.base)
	defer cancel()

	cmd, stdin, stdout, stderr, err := c.start()
	if err != nil {
		c.log.Error("Could not start hermes acp", "error", err)
		_ = c.conn.Close(websocket.StatusInternalError, "could not start hermes acp")
		return
	}
	c.log.Info("Started hermes acp", "pid", cmd.Process.Pid)

	exited := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		c.logStderr(stderr)
	}()
	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		c.pumpStdout(ctx, stdout)
	}()
	wsDone := make(chan struct{})
	go func() {
		defer close(wsDone)
		c.pumpWebSocket(ctx, stdin)
	}()
	go c.ping(ctx)

	select {
	case <-wsDone:
		c.stop(cmd, stdin, exited)
	case <-exited:
	case <-stdoutDone:
		c.stop(cmd, stdin, exited)
	}
	_ = stdin.Close()
	killGroup(cmd, syscall.SIGKILL)
	select {
	case <-stdoutDone:
	case <-time.After(time.Second):
	}
	_ = stdout.Close()
	_ = stderr.Close()
	<-stderrDone
	c.srv.untrack(cmd.Process.Pid)
	c.log.Info("hermes acp exited", "status", exitStatus(waitErr))
	_ = c.conn.Close(CloseChildExited, "hermes acp exited")
	cancel()
	<-wsDone
}

// start runs `<hermes> acp` in its own process group, with the host's
// environment. The pipes are plain files so Wait does not wait on them.
func (c *connection) start() (*exec.Cmd, *os.File, *os.File, *os.File, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW)
		return nil, nil, nil, nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW, outR, outW)
		return nil, nil, nil, nil, err
	}
	//nolint:gosec,noctx // the hermes path is operator config; stop ends the process group with a grace period, which a context kill would cut short
	cmd := exec.Command(c.srv.opts.Hermes, "acp")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.srv.mu.Lock()
	err = cmd.Start()
	if err == nil {
		c.srv.children[cmd.Process.Pid] = true
	}
	c.srv.mu.Unlock()
	closeAll(inR, outW, errW)
	if err != nil {
		closeAll(inW, outR, errR)
		return nil, nil, nil, nil, err
	}
	return cmd, inW, outR, errR, nil
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// stop ends the child as section 6.1 says: close stdin, SIGTERM to its
// process group after StopGrace, SIGKILL after KillGrace more.
func (c *connection) stop(cmd *exec.Cmd, stdin *os.File, exited <-chan struct{}) {
	_ = stdin.Close()
	select {
	case <-exited:
		return
	case <-time.After(c.srv.opts.StopGrace):
	}
	c.log.Info("hermes acp still running after stdin closed; sending SIGTERM")
	killGroup(cmd, syscall.SIGTERM)
	select {
	case <-exited:
		return
	case <-time.After(c.srv.opts.KillGrace):
	}
	c.log.Warn("hermes acp ignored SIGTERM; sending SIGKILL")
	killGroup(cmd, syscall.SIGKILL)
	<-exited
}

func killGroup(cmd *exec.Cmd, sig syscall.Signal) {
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

func exitStatus(err error) string {
	if err == nil {
		return "0"
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.String()
	}
	return err.Error()
}

// pumpWebSocket writes each WebSocket message to the child's stdin as one
// compact line, after the editor-side rewrites.
func (c *connection) pumpWebSocket(ctx context.Context, stdin io.Writer) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			if status := websocket.CloseStatus(err); status != -1 {
				c.log.Info("ACP leg closed", "code", int(status))
			} else if ctx.Err() == nil {
				c.log.Info("ACP leg lost", "error", err)
			}
			return
		}
		if typ != websocket.MessageText {
			c.log.Warn("Dropped a binary WebSocket message")
			continue
		}
		c.mu.Lock()
		line, err := c.rw.fromEditor(data)
		c.mu.Unlock()
		if err != nil {
			var re *rpcError
			if errors.As(err, &re) {
				c.log.Warn("Refused an editor message", "error", re.message)
				if re.notify {
					continue
				}
				if werr := c.conn.Write(ctx, websocket.MessageText, re.response()); werr != nil {
					return
				}
			}
			continue
		}
		if _, err := stdin.Write(append(line, '\n')); err != nil {
			c.log.Info("hermes acp stdin closed", "error", err)
			return
		}
	}
}

// pumpStdout sends each JSON line of the child's stdout as one WebSocket
// message, after the agent-side rewrites.
func (c *connection) pumpStdout(ctx context.Context, stdout io.Reader) {
	br := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, err := readLine(br, maxMessage)
		if len(line) > 0 {
			if !json.Valid(line) {
				c.log.Warn("Dropped a stdout line that is not JSON", "bytes", len(line))
			} else {
				c.mu.Lock()
				out, rerr := c.rw.fromAgent(line)
				c.mu.Unlock()
				if rerr != nil {
					c.log.Error("Could not read the bindings file; listing no sessions", "error", rerr)
				}
				if out != nil {
					if werr := c.conn.Write(ctx, websocket.MessageText, out); werr != nil {
						return
					}
				}
			}
		}
		if errors.Is(err, errLineTooLong) {
			c.log.Error("Dropped a stdout line over the size limit", "limit", maxMessage)
			continue
		}
		if err != nil {
			return
		}
	}
}

var errLineTooLong = errors.New("line too long")

// readLine reads one line without its line ending. A line over limit is
// skipped and reported with errLineTooLong.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	tooLong := false
	for {
		frag, err := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(frag) > limit+2 {
				tooLong, line = true, nil
			} else {
				line = append(line, frag...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			if err == nil {
				err = errLineTooLong
			}
			return nil, err
		}
		for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
			line = line[:len(line)-1]
		}
		return line, err
	}
}

func (c *connection) logStderr(stderr io.Reader) {
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		c.log.Info("hermes acp: " + sc.Text())
	}
	if err := sc.Err(); err != nil {
		c.log.Warn("hermes acp stderr line too long; dropping the rest of its stderr", "error", err)
	}
	_, _ = io.Copy(io.Discard, stderr)
}

func (c *connection) ping(ctx context.Context) {
	t := time.NewTicker(c.srv.opts.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, protocol.PingTimeout)
		err := c.conn.Ping(pctx)
		cancel()
		if err != nil && ctx.Err() == nil {
			c.log.Warn("No pong from the hub; closing", "error", err)
			_ = c.conn.Close(protocol.ClosePingTimeout, "ping timeout")
			return
		}
	}
}
