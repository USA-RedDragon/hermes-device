//go:build unix

package device_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/USA-RedDragon/talaria/protocol"
	"github.com/coder/websocket"
)

func TestDisconnectKillsCommands(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pidFile := filepath.Join(root, "pid")
	f := &fakeHub{}
	var first atomic.Bool
	f.handler = func(ctx context.Context, conn *websocket.Conn, _ protocol.Hello) {
		if !first.CompareAndSwap(false, true) {
			<-ctx.Done()
			return
		}
		writeJSON(ctx, conn, protocol.Exec{Type: protocol.TypeExec, ID: "1", Cmd: "echo $$ > " + pidFile + "; echo go; sleep 60", Cwd: root, Timeout: 120})
		for {
			typ, _, err := readMsg(ctx, conn)
			if err != nil || typ == protocol.TypeOutput {
				return
			}
		}
	}
	srv := serve(t, f)
	start(t, options(t, srv.URL, root))
	waitFor(t, "a reconnect", func() bool { return f.conns.Load() >= 2 })
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the command to die", func() bool { return syscall.Kill(pid, 0) != nil })
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }
