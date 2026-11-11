package executor

import (
	"context"
	"testing"
	"time"
)

func TestDetachLiftsTheTimeout(t *testing.T) {
	t.Parallel()
	for _, detach := range []bool{false, true} {
		p, err := start("bash", Request{Cmd: "sleep 0.6; echo done", Cwd: t.TempDir(), Timeout: 100 * time.Millisecond},
			func([]byte) {}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if detach {
			p.Detach()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		res, err := p.Wait(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if res.Killed == detach {
			t.Errorf("detach %v: result %+v", detach, res)
		}
	}
}
