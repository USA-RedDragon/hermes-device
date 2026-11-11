package device

import (
	"encoding/json"
	"syscall"
	"testing"

	"github.com/USA-RedDragon/hermes-device/internal/executor"
)

func TestExitMessage(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		res  executor.Result
		want string
	}{
		"exit":   {executor.Result{Code: 3}, `{"type":"exit","id":"r1","code":3}`},
		"killed": {executor.Result{Code: 130, Killed: true, Signal: syscall.SIGTERM}, `{"type":"exit","id":"r1","code":130,"killed":true,"signal":"SIGTERM"}`},
		"signal": {executor.Result{Code: 139, Signal: syscall.SIGSEGV}, `{"type":"exit","id":"r1","code":139,"signal":"SIGSEGV"}`},
		"oom": {
			executor.Result{Code: 137, Signal: syscall.SIGKILL, OOM: true, MemoryMax: 4 << 30},
			`{"type":"exit","id":"r1","code":137,"signal":"SIGKILL","reason":"oom","memory_max":4294967296}`,
		},
	}
	for name, c := range cases {
		got, err := json.Marshal(exitMessage("r1", c.res))
		if err != nil || string(got) != c.want {
			t.Errorf("%s: %s, %v; want %s", name, got, err, c.want)
		}
	}
}
