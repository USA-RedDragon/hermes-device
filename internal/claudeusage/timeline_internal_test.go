package claudeusage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	acctB    = "acct-b"
	orgB     = "org-b"
	emailA   = "alice@example.com"
	emailB   = "bob@example.com"
	sentinel = "SECRET-OAUTH-VALUE"
)

func setAccount(t *testing.T, path, uuid, org, email string, mod time.Time) {
	t.Helper()
	body := marshal(map[string]any{
		"numStartups": 3,
		"oauthAccount": map[string]any{
			accountKey: uuid, orgKey: org, "emailAddress": email, "accessToken": sentinel,
		},
	})
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func tlCollector(dir, state string, clk *clock) *Collector {
	return New(Options{Sources: []Source{{Dir: dir, ConfigFile: filepath.Join(dir, "cfg.json")}}, StateFile: state,
		ReadRate: 1 << 40, Now: clk.now})
}

func accountsOf(r Report) map[string]uint64 {
	out := map[string]uint64{}
	for _, x := range r.Rows {
		out[x.Account] += x.Requests
	}
	return out
}

func TestSessionSpanningTwoAccountsIsSplit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state := filepath.Join(t.TempDir(), "state.gob")
	cfg := filepath.Join(dir, "cfg.json")
	h := now().Truncate(time.Hour)
	clk := &clock{t: h.Add(10 * time.Minute)}
	setAccount(t, cfg, acctA, orgA, emailA, h)
	c := tlCollector(dir, state, clk)
	c.Observe()
	clk.t = h.Add(30 * time.Minute)
	setAccount(t, cfg, acctB, orgB, emailB, h.Add(20*time.Minute))
	c.Observe()
	clk.t = h.Add(40 * time.Minute)
	path := filepath.Join(dir, "projects", "p", "s1.jsonl")
	write(t, path, ownerLine("s1", "owner"),
		base("s1", "msg_1", "r1", h.Add(11*time.Minute)).json(),
		base("s1", "msg_2", "r2", h.Add(19*time.Minute)).json(),
		base("s1", "msg_3", "r3", h.Add(21*time.Minute)).json(),
		base("s1", "msg_4", "r4", h.Add(35*time.Minute)).json(),
		base("s1", "msg_5", "r5", h.Add(5*time.Minute)).json())
	scan(t, c)
	rep := c.Report()
	got := accountsOf(rep)
	if got[acctA] != 2 || got[acctB] != 2 || got["owner"] != 1 || len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for _, r := range rep.Rows {
		if r.Account == acctB && r.Org != orgB {
			t.Errorf("org %q", r.Org)
		}
		if r.Hour != h.Unix() {
			t.Errorf("hour %d", r.Hour)
		}
	}
}

func TestTimelineFallsBackToSessionOwner(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "cfg.json")
	h := now().Truncate(time.Hour)
	clk := &clock{t: h.Add(30 * time.Minute)}
	setAccount(t, cfg, acctB, orgB, emailB, h)
	c := tlCollector(dir, filepath.Join(t.TempDir(), "state.gob"), clk)
	write(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), ownerLine("s1", acctA),
		base("s1", "msg_1", "r1", h.Add(10*time.Minute)).json(), base("s1", "msg_2", "r2", h.Add(40*time.Minute)).json())
	write(t, filepath.Join(dir, "projects", "p", "s2.jsonl"), base("s2", "msg_3", "r3", h.Add(10*time.Minute)).json())
	scan(t, c)
	got := accountsOf(c.Report())
	if got[acctA] != 1 || got[acctB] != 1 || got[""] != 1 {
		t.Fatalf("got %v", got)
	}
}

func TestTimelineIgnoresSlotsAndUnreadableConfig(t *testing.T) {
	t.Parallel()
	slot, plain := t.TempDir(), t.TempDir()
	h := now().Truncate(time.Hour)
	setAccount(t, filepath.Join(slot, ".claude.json"), acctB, orgB, emailB, h)
	if err := os.WriteFile(filepath.Join(plain, ".claude.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: h.Add(time.Minute)}
	c := New(Options{Sources: []Source{{Dir: slot, Slot: "w", OwnAccount: true}, {Dir: plain}},
		StateFile: filepath.Join(t.TempDir(), "state.gob"), ReadRate: 1 << 40, Now: clk.now})
	if c.Observe() || len(c.tl.snapshot()) != 0 {
		t.Fatalf("timeline %+v", c.tl.snapshot())
	}
	write(t, filepath.Join(slot, "projects", "p", "s1.jsonl"), base("s1", "msg_1", "r1", h.Add(time.Second)).json())
	scan(t, c)
	if got := accountsOf(c.Report()); got[acctB] != 1 {
		t.Fatalf("got %v", got)
	}
}
