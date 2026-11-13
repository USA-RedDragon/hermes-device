//go:build unix

package claudeusage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTimelinePersistsAcrossRestarts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state := filepath.Join(t.TempDir(), "state.gob")
	cfg := filepath.Join(dir, "cfg.json")
	h := now().Truncate(time.Hour)
	clk := &clock{t: h.Add(time.Minute)}
	setAccount(t, cfg, acctA, orgA, emailA, h)
	c := tlCollector(dir, state, clk)
	c.Observe()
	file := filepath.Join(filepath.Dir(state), timelineName)
	data, err := os.ReadFile(file)
	if err != nil || strings.Contains(string(data), sentinel) || !strings.Contains(string(data), emailA) {
		t.Fatalf("timeline file %q %v", data, err)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}

	clk.t = h.Add(30 * time.Minute)
	d := tlCollector(dir, state, clk)
	setAccount(t, cfg, acctB, orgB, emailB, h.Add(20*time.Minute))
	d.Observe()
	d.Observe()
	e := tlCollector(dir, state, clk)
	e.Observe()
	list := e.tl.snapshot()[dir]
	if len(list) != 2 || list[0].Account != acctA || list[0].AtMS != h.Add(time.Minute).UnixMilli() ||
		list[1].Account != acctB || list[1].AtMS != h.Add(20*time.Minute).UnixMilli() {
		t.Fatalf("timeline %+v", list)
	}
	write(t, filepath.Join(dir, "projects", "p", "s1.jsonl"),
		base("s1", "msg_1", "r1", h.Add(10*time.Minute)).json(), base("s1", "msg_2", "r2", h.Add(25*time.Minute)).json())
	scan(t, e)
	if got := accountsOf(e.Report()); got[acctA] != 1 || got[acctB] != 1 {
		t.Fatalf("got %v", got)
	}
}
