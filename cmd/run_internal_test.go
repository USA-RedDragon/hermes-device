package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/config"
	"github.com/USA-RedDragon/hermes-device/internal/daemonstate"
	"github.com/USA-RedDragon/hermes-device/internal/device"
	"github.com/USA-RedDragon/hermes-device/internal/hubapi"
	"github.com/USA-RedDragon/hermes-device/internal/splice"
	"github.com/USA-RedDragon/hermes-device/internal/tokenstore"
	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/coder/websocket"
)

const (
	testToken   = "hdt_run"
	testUser    = "alice"
	testDevice  = "desktop"
	whoID       = "d_who"
	pairedID    = "d_1"
	pairedToken = "hdt_new"
	pairedName  = "laptop"
)

func writeJSONBody(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode: %v", err)
	}
}

type runHub struct {
	t         *testing.T
	whoami    int
	closeCode websocket.StatusCode
	hellos    atomic.Int32
}

func (h *runHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case hubapi.PathWhoami:
		if h.whoami != 0 {
			w.WriteHeader(h.whoami)
			_, _ = w.Write([]byte(`{"error":"x"}`))
			return
		}
		writeJSONBody(h.t, w, hubapi.Whoami{DeviceID: whoID, DeviceName: testDevice, User: testUser, Backend: testUser})
	case hubapi.PathExecLeg:
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		h.hellos.Add(1)
		b, err := json.Marshal(protocol.Welcome{Type: protocol.TypeWelcome, Version: protocol.Version, DeviceID: whoID, Name: testDevice})
		if err != nil {
			h.t.Error(err)
			return
		}
		_ = conn.Write(ctx, websocket.MessageText, b)
		if h.closeCode != 0 {
			_ = conn.Close(h.closeCode, "test")
			return
		}
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	default:
		http.NotFound(w, r)
	}
}

type fakeSplice struct {
	ln     chan net.Listener
	result error
}

func (f *fakeSplice) Serve(ctx context.Context, ln net.Listener) error {
	f.ln <- ln
	if f.result != nil {
		return f.result
	}
	<-ctx.Done()
	return nil
}

func (f *fakeSplice) ControlSecret() string { return "control-secret" }

type runSetup struct {
	cfg      *config.Config
	stateDir string
	splice   *fakeSplice
	hub      *runHub
}

func setupRun(t *testing.T, hub *runHub, spliceResult error) *runSetup { //nolint:unparam // kept for splice failure cases
	t.Helper()
	hub.t = t
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	stateDir := filepath.Join(t.TempDir(), "state")
	cfg := &config.Config{
		LogLevel: "debug", PingInterval: 25 * time.Second,
		Hub:       config.Hub{URL: srv.URL},
		Store:     config.Store{StateDir: stateDir},
		Device:    config.Device{Roots: []string{t.TempDir()}, Shell: "bash"},
		Gateway:   config.Gateway{Listen: "127.0.0.1:0"},
		Reconnect: config.Reconnect{MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond},
	}
	st, _, err := tokenStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Save(tokenstore.Record{Hub: srv.URL, Token: testToken, DeviceID: "d_rec", DeviceName: testDevice}); err != nil {
		t.Fatal(err)
	}
	return &runSetup{cfg: cfg, stateDir: stateDir, splice: &fakeSplice{ln: make(chan net.Listener, 1), result: spliceResult}, hub: hub}
}

func (s *runSetup) start(ctx context.Context) chan error {
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, s.cfg, "v0.2.0", "hermes-device/test", func(opts splice.Options) spliceServer {
			if opts.Hub == nil || opts.PingInterval != s.cfg.PingInterval {
				s.hub.t.Errorf("splice options = %+v", opts)
			}
			return s.splice
		})
	}()
	return done
}

func waitErr(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("runDaemon did not return")
		return nil
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunWritesAndRemovesDaemonState(t *testing.T) {
	t.Parallel()
	s := setupRun(t, &runHub{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := s.start(ctx)
	var ln net.Listener
	select {
	case ln = <-s.splice.ln:
	case <-time.After(10 * time.Second):
		t.Fatal("splice never served")
	}
	waitUntil(t, "the exec leg", func() bool { return s.hub.hellos.Load() >= 1 })
	st, err := daemonstate.Read(s.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	want := daemonstate.State{PID: os.Getpid(), Splice: ln.Addr().String(), ControlSecret: "control-secret", DeviceID: whoID, Hub: s.cfg.Hub.URL}
	if *st != want {
		t.Errorf("state = %+v, want %+v", *st, want)
	}
	if host, _, _ := net.SplitHostPort(st.Splice); host != "127.0.0.1" {
		t.Errorf("splice address %s is not loopback", st.Splice)
	}
	fi, err := os.Stat(filepath.Join(s.stateDir, daemonstate.FileName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, %v", fi, err)
	}
	cancel()
	if err := waitErr(t, done); err != nil {
		t.Errorf("runDaemon = %v", err)
	}
	if _, err := daemonstate.Read(s.stateDir); !errors.Is(err, daemonstate.ErrNotRunning) {
		t.Errorf("state after exit: %v", err)
	}
}

func TestRunWhoamiUnauthorizedFailsFast(t *testing.T) {
	t.Parallel()
	s := setupRun(t, &runHub{whoami: http.StatusUnauthorized}, nil)
	err := waitErr(t, s.start(context.Background()))
	if !errors.Is(err, hubapi.ErrUnauthorized) {
		t.Errorf("runDaemon = %v", err)
	}
	if n := s.hub.hellos.Load(); n != 0 {
		t.Errorf("exec leg dialed %d times", n)
	}
	if _, err := daemonstate.Read(s.stateDir); !errors.Is(err, daemonstate.ErrNotRunning) {
		t.Errorf("state written: %v", err)
	}
}

func TestRunWhoamiOtherErrorContinues(t *testing.T) {
	t.Parallel()
	s := setupRun(t, &runHub{whoami: http.StatusBadGateway}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := s.start(ctx)
	waitUntil(t, "the exec leg", func() bool { return s.hub.hellos.Load() >= 1 })
	waitUntil(t, "the state file", func() bool {
		st, err := daemonstate.Read(s.stateDir)
		return err == nil && st.DeviceID == "d_rec"
	})
	cancel()
	if err := waitErr(t, done); err != nil {
		t.Errorf("runDaemon = %v", err)
	}
}

func TestRunStopsWhenReplaced(t *testing.T) {
	t.Parallel()
	s := setupRun(t, &runHub{closeCode: protocol.CloseReplaced}, nil)
	err := waitErr(t, s.start(context.Background()))
	if !errors.Is(err, device.ErrReplaced) {
		t.Errorf("runDaemon = %v", err)
	}
	if _, err := daemonstate.Read(s.stateDir); !errors.Is(err, daemonstate.ErrNotRunning) {
		t.Errorf("state after exit: %v", err)
	}
}

func TestRunWithoutToken(t *testing.T) {
	t.Parallel()
	s := setupRun(t, &runHub{}, nil)
	s.cfg.Store.StateDir = t.TempDir()
	if err := waitErr(t, s.start(context.Background())); !errors.Is(err, tokenstore.ErrNoToken) {
		t.Errorf("runDaemon = %v", err)
	}
}
