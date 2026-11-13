//go:build unix

package cmd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/USA-RedDragon/talaria/internal/daemonstate"
)

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

func TestRunTokenFileRules(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		body string
		mode os.FileMode
		want error
	}{
		"readable by others": {testToken, 0o644, ErrTokenFileMode},
		"empty":              {" \n", 0o600, ErrEmptyToken},
		"missing":            {"", 0, os.ErrNotExist},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := setupRun(t, &runHub{}, nil)
			s.cfg.Hub.TokenFile = filepath.Join(t.TempDir(), "token")
			if tc.mode != 0 {
				if err := os.WriteFile(s.cfg.Hub.TokenFile, []byte(tc.body), tc.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(s.cfg.Hub.TokenFile, tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			if err := waitErr(t, s.start(context.Background())); !errors.Is(err, tc.want) {
				t.Errorf("runDaemon = %v, want %v", err, tc.want)
			}
			if n := s.hub.hellos.Load(); n != 0 {
				t.Errorf("exec leg dialed %d times", n)
			}
		})
	}
}
