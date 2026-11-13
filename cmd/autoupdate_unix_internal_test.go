//go:build unix

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/autoupdate"
	"github.com/USA-RedDragon/talaria/internal/config"
	"github.com/USA-RedDragon/talaria/protocol"
	"github.com/coder/websocket"
)

func TestDaemonUpdaterDefaults(t *testing.T) {
	t.Parallel()
	ok := func(context.Context, string, string, []byte) (string, error) { return "x", nil }
	cfg := defaultConfig(t, map[string]string{"TALARIA_HUB_URL": "https://hub.example.com"})
	if !cfg.Update.Auto || cfg.Update.Interval != 6*time.Hour {
		t.Fatalf("defaults: %+v", cfg.Update)
	}
	b, _ := autoBase(t, verNow, ok)
	if d := b.daemonUpdater(cfg); d == nil || d.Interval != 6*time.Hour || d.Version != verNow {
		t.Fatalf("desktop device: %+v", d)
	}
	for name, change := range map[string]func(*base, *config.Config){
		"update.auto false": func(_ *base, c *config.Config) { c.Update.Auto = false },
		"hub.token":         func(_ *base, c *config.Config) { c.Hub.Token = "hdt_x" },
		"hub.token-file":    func(_ *base, c *config.Config) { c.Hub.TokenFile = "/run/secrets/token" },
		"dev build":         func(b *base, _ *config.Config) { b.version = "dev" },
		"no service":        func(b *base, _ *config.Config) { b.underService = func() bool { return false } },
		"other binary":      func(b *base, _ *config.Config) { b.executable = os.Executable },
	} {
		b, _ := autoBase(t, verNow, ok)
		c := *cfg
		change(b, &c)
		if d := b.daemonUpdater(&c); d != nil {
			t.Errorf("%s: auto-update on", name)
		}
	}
}

// runDaemon with auto-update installs, waits until the device is idle and
// no TUI is open, and then stops with restartError.
func TestRunRestartsAfterAnUpdateWhenIdle(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	busy := make(chan struct{})
	var once sync.Once
	hub := &runHub{}
	var root string
	hub.exec = func(ctx context.Context, conn *websocket.Conn) {
		writeJSONMsg(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Cmd: "echo started; sleep 30", Cwd: root, Timeout: 60})
		go func() {
			<-release
			writeJSONMsg(ctx, conn, protocol.Kill{Type: protocol.TypeKill, ID: "1"})
		}()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var p protocol.Ping
			_ = json.Unmarshal(data, &p)
			switch p.Type {
			case protocol.TypePing:
				writeJSONMsg(ctx, conn, protocol.Ping{Type: protocol.TypePong, Seq: p.Seq})
			case protocol.TypeOutput:
				once.Do(func() { close(busy) })
			}
		}
	}
	s := setupRun(t, hub, nil)
	root = s.cfg.Device.Roots[0]
	s.cfg.PingInterval = 50 * time.Millisecond
	var installed atomic.Int32
	s.splice.tuis.Store(1)
	s.au = &autoupdate.Daemon{
		Version: verNow, Interval: time.Hour, FirstMin: time.Millisecond, FirstMax: time.Millisecond,
		IdleWait: time.Hour, IdlePoll: 20 * time.Millisecond,
		Latest:    func(context.Context) (string, error) { return tagNext, nil },
		Installed: func(context.Context) (string, error) { return verNow, nil },
		Install: func(context.Context, string) error {
			<-busy
			installed.Add(1)
			return nil
		},
	}
	done := s.start(t.Context())
	waitUntil(t, "the install", func() bool { return installed.Load() == 1 })
	var tuiClosed atomic.Int64
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(release)
		time.Sleep(300 * time.Millisecond)
		tuiClosed.Store(time.Now().UnixNano())
		s.splice.tuis.Store(0)
	}()
	err := waitErr(t, done)
	var restart *restartError
	if !errors.As(err, &restart) || restart.tag != tagNext {
		t.Fatalf("runDaemon = %v", err)
	}
	if tuiClosed.Load() == 0 {
		t.Error("restarted while a command ran or a TUI was open")
	}
	if _, err := os.Stat(filepath.Join(s.stateDir, "daemon.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("daemon state left behind: %v", err)
	}
}

func writeJSONMsg(ctx context.Context, conn *websocket.Conn, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	_ = conn.Write(ctx, websocket.MessageText, b)
}
