package claudeusage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	secret     = "SECRET-MESSAGE-TEXT"
	opus       = "claude-opus-5-5"
	app        = "app"
	acctA      = "acct-a"
	orgA       = "org-a"
	typeKey    = "type"
	acctW      = "acct-w"
	slotSpare  = "spare"
	slotExtra  = "extra"
	accountKey = "accountUuid"
	orgKey     = "organizationUuid"
)

// nowUnix is the tests' clock: 2026-11-10 18:30 UTC.
const nowUnix = 1794335400

func now() time.Time { return time.Unix(nowUnix, 0).UTC() }

func marshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

type usageLine struct {
	session, msg, req, model, cwd string
	at                            time.Time
	in, out, cw, cr               uint64
	cw1h                          uint64
	sidechain, noReq, noSession   bool
	speed                         string
}

func (u usageLine) json() string {
	usage := map[string]any{
		"input_tokens": u.in, "output_tokens": u.out, "cache_creation_input_tokens": u.cw, "cache_read_input_tokens": u.cr,
		"speed": u.speed,
	}
	if u.cw1h > 0 {
		usage["cache_creation"] = map[string]any{"ephemeral_5m_input_tokens": u.cw - u.cw1h, "ephemeral_1h_input_tokens": u.cw1h}
	}
	m := map[string]any{
		typeKey: "assistant", "timestamp": u.at.Format(time.RFC3339Nano), "version": "2.1.322", "cwd": u.cwd,
		"isSidechain": u.sidechain, "requestedModel": u.model,
		"message": map[string]any{
			"id": u.msg, "model": u.model, "role": "assistant",
			"content": []any{map[string]any{typeKey: "text", "text": secret}},
			"usage":   usage,
		},
	}
	if !u.noSession {
		m["sessionId"] = u.session
	}
	if !u.noReq {
		m["requestId"] = u.req
	}
	return marshal(m)
}

func ownerLine(session, acct string) string {
	org := orgA
	return marshal(map[string]any{typeKey: "session-owner", "sessionId": session, accountKey: acct, orgKey: org})
}

func userLine(session string) string {
	return marshal(map[string]any{typeKey: "user", "sessionId": session, "message": map[string]any{"role": "user", "content": secret}})
}

func base(session, msg, req string, at time.Time) usageLine {
	return usageLine{session: session, msg: msg, req: req, model: opus, cwd: "/home/alice/src/app", at: at,
		in: 10, out: 100, cw: 1000, cr: 10000}
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func newCollector(t *testing.T, dir string, sources ...Source) *Collector {
	t.Helper()
	if len(sources) == 0 {
		sources = []Source{{Dir: dir}}
	}
	return New(Options{Sources: sources, StateFile: filepath.Join(t.TempDir(), "state.gob"), ReadRate: 1 << 40,
		Now: now})
}

func scan(t *testing.T, c *Collector) {
	t.Helper()
	if _, err := c.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type sums struct{ in, out, cw, cw1h, cr, req uint64 }

func total(r Report) sums {
	var s sums
	for _, x := range r.Rows {
		s.in += x.Input
		s.out += x.Output
		s.cw += x.CacheWrite
		s.cw1h += x.CacheWrite1h
		s.cr += x.CacheRead
		s.req += x.Requests
	}
	return s
}

func TestParseReadsOnlyUsageFields(t *testing.T) {
	t.Parallel()
	u := base("s1", "msg_1", "req_1", now())
	u.speed, u.cw1h = "fast", 400
	recs, own := parseLine([]byte(u.json()), "file")
	if own != nil || len(recs) != 1 {
		t.Fatalf("got %v %v", recs, own)
	}
	r := recs[0]
	if r.Model != "claude-opus-5-5-fast" || r.Project != app || r.Session != "s1" || r.MessageID != "msg_1" ||
		r.RequestID != "req_1" || r.Tokens != (tokens{10, 100, 1000, 400, 10000}) || !r.At.Equal(now()) {
		t.Fatalf("record %+v", r)
	}
	if strings.Contains(fmt.Sprintf("%+v", r), secret) {
		t.Fatal("record holds message text")
	}
	if recs, own := parseLine([]byte(userLine("s1")), "f"); recs != nil || own != nil {
		t.Fatal("user line counted")
	}
	_, own = parseLine([]byte(ownerLine("s1", acctA)), "f")
	if own == nil || *own != (owner{"s1", acctA, orgA}) {
		t.Fatalf("owner %+v", own)
	}
}

func TestParseDropsWhatCcusageDrops(t *testing.T) {
	t.Parallel()
	for name, line := range map[string]string{
		"bad version":   strings.Replace(base("s", "m", "r", now()).json(), `"2.1.322"`, `"dev"`, 1),
		"empty session": base("", "m", "r", now()).json(),
		"empty request": base("s", "m", "", now()).json(),
		"empty id":      base("s", "", "r", now()).json(),
		"no input":      strings.Replace(base("s", "m", "r", now()).json(), `"input_tokens":10,`, "", 1),
		"bad time":      strings.Replace(base("s", "m", "r", now()).json(), now().Format(time.RFC3339Nano), "yesterday", 1),
		"not json":      `{"usage":{`,
	} {
		if recs, _ := parseLine([]byte(line), "f"); recs != nil {
			t.Errorf("%s: counted", name)
		}
	}
	u := base("s", "m", "r", now())
	u.model = "<synthetic>"
	if recs, _ := parseLine([]byte(u.json()), "f"); len(recs) != 1 || recs[0].Model != "" {
		t.Fatalf("synthetic: %+v", recs)
	}
}

func TestParseAdvisorIterations(t *testing.T) {
	t.Parallel()
	line := base("s", "msg_1", "r", now()).json()
	line = strings.Replace(line, `"input_tokens":10,`, `"input_tokens":10,"iterations":[{"type":"message","input_tokens":1,"output_tokens":1},`+
		`{"type":"advisor_message","model":"claude-haiku-5-5","input_tokens":7,"output_tokens":3,"cache_read_input_tokens":2}],`, 1)
	recs, _ := parseLine([]byte(line), "f")
	if len(recs) != 2 || recs[1].Model != "claude-haiku-5-5" || recs[1].MessageID != "msg_1:advisor:0" || recs[1].Tokens.Input != 7 ||
		recs[1].Tokens.CacheRead != 2 {
		t.Fatalf("records %+v", recs)
	}
}

func TestProject(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"/home/bob/src/site/": "site", "": "unknown", "/": "unknown", `C:\Users\bob\proj`: "proj"} {
		if got := project(in); got != want {
			t.Errorf("project(%q) = %q, want %q", in, got, want)
		}
	}
	if got := fileSession("/x/projects/p/sess-1/subagents/agent-1.jsonl"); got != "sess-1" {
		t.Errorf("subagent session %q", got)
	}
	if got := fileSession("/x/projects/p/sess-2.jsonl"); got != "sess-2" {
		t.Errorf("session %q", got)
	}
}

func TestDedupeContentBlocksKeepsLargest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := base("s1", "msg_1", "req_1", now())
	a.out = 5
	b := a
	b.out = 100
	write(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), a.json(), b.json(), a.json())
	c := newCollector(t, dir)
	scan(t, c)
	if got := total(c.Report()); got.out != 100 || got.req != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestDedupeAcrossFilesAndSessions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := base("s1", "msg_1", "req_1", now())
	write(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), a.json())
	copyA := a
	copyA.session = "s2"
	write(t, filepath.Join(dir, "projects", "p", "s2.jsonl"), copyA.json(), base("s2", "msg_2", "req_2", now()).json())
	c := newCollector(t, dir)
	scan(t, c)
	if got := total(c.Report()); got.req != 2 || got.out != 200 {
		t.Fatalf("got %+v", got)
	}
}

func TestDedupeRequestless(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m1 := base("s1", "msg_1", "", now())
	m1.noReq = true
	m1b := m1
	m1b.at = now().Add(time.Second)
	g1 := base("s1", "gw-1", "", now())
	g1.noReq = true
	g1b := g1
	g1b.at = now().Add(time.Second)
	other := m1
	other.session = "s2"
	write(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), m1.json(), m1b.json(), g1.json(), g1b.json(), other.json())
	c := newCollector(t, dir)
	scan(t, c)
	// msg_1 collapses within s1 across timestamps; gw-1 does not; s2 is separate.
	if got := total(c.Report()); got.req != 4 {
		t.Fatalf("got %+v", got)
	}
}

func TestDedupeSidechainReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parent := base("s1", "msg_1", "req_1", now())
	replay := parent
	replay.req, replay.sidechain, replay.cr = "req_9", true, 99999
	own := base("s1", "msg_2", "req_2", now())
	own.sidechain = true
	write(t, filepath.Join(dir, "projects", "p", "s1", "subagents", "agent-1.jsonl"), replay.json(), own.json())
	write(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), parent.json())
	c := newCollector(t, dir)
	scan(t, c)
	got := total(c.Report())
	if got.req != 2 || got.cr != 20000 {
		t.Fatalf("got %+v", got)
	}
	var side uint64
	for _, r := range c.Report().Rows {
		if r.Sidechain {
			side += r.Requests
		}
	}
	if side != 1 {
		t.Fatalf("sidechain requests %d, want 1: the main-thread copy wins", side)
	}
}

func TestOffsetsAppendPartialTruncateRotate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "projects", "p", "s1.jsonl")
	write(t, path, base("s1", "msg_1", "r1", now()).json())
	c := newCollector(t, dir)
	scan(t, c)
	if changed, _ := c.Scan(context.Background()); changed {
		t.Fatal("an unchanged file counted again")
	}
	half := base("s1", "msg_2", "r2", now()).json()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(half[:40])
	_ = f.Close()
	scan(t, c)
	if got := total(c.Report()); got.req != 1 {
		t.Fatalf("partial line counted: %+v", got)
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(half[40:] + "\n")
	_ = f.Close()
	scan(t, c)
	if got := total(c.Report()); got.req != 2 {
		t.Fatalf("completed line: %+v", got)
	}
	if err := os.WriteFile(path, []byte(base("s1", "msg_3", "r3", now()).json()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	scan(t, c)
	if got := total(c.Report()); got.req != 3 {
		t.Fatalf("after truncation: %+v", got)
	}
	tmp := path + ".new"
	write(t, tmp, base("s1", "msg_3", "r3", now()).json(), base("s1", "msg_4", "r4", now()).json(), base("s1", "msg_5", "r5", now()).json())
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	scan(t, c)
	if got := total(c.Report()); got.req != 5 {
		t.Fatalf("after rotation: %+v", got)
	}
}

func TestLongLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	long := base("s1", "msg_1", "r1", now()).json()
	long = strings.Replace(long, secret, strings.Repeat("x", 3<<20), 1)
	path := filepath.Join(dir, "projects", "p", "s1.jsonl")
	write(t, path, long, base("s1", "msg_2", "r2", now()).json())
	c := newCollector(t, dir)
	scan(t, c)
	if got := total(c.Report()); got.req != 2 {
		t.Fatalf("got %+v", got)
	}
	fi, _ := os.Stat(path)
	if st := c.files[path]; st.Offset != fi.Size() {
		t.Fatalf("offset %d, size %d", st.Offset, fi.Size())
	}
}

func TestAggregationOwnersAndHours(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	h := now().Truncate(time.Hour)
	write(t, filepath.Join(dir, "projects", "p", "s1.jsonl"),
		base("s1", "msg_1", "r1", h.Add(5*time.Minute)).json(), ownerLine("s1", acctA), userLine("s1"),
		base("s1", "msg_2", "r2", h.Add(-time.Minute)).json())
	write(t, filepath.Join(dir, "projects", "p", "s1", "subagents", "agent-1.jsonl"), func() string {
		u := base("s1", "msg_3", "r3", h.Add(time.Minute))
		u.sidechain = true
		return u.json()
	}())
	write(t, filepath.Join(dir, "projects", "q", "s2.jsonl"), base("s2", "msg_4", "r4", h.Add(time.Minute)).json())
	old := base("s2", "msg_5", "r5", now().Add(-Retention-time.Hour))
	write(t, filepath.Join(dir, "projects", "q", "s2.jsonl"), old.json())
	c := newCollector(t, dir)
	scan(t, c)
	rows := c.Report().Rows
	want := map[RowKey]uint64{
		{Hour: h.Add(-time.Hour).Unix(), Account: acctA, Org: orgA, Model: opus, Project: app}:  1,
		{Hour: h.Unix(), Account: acctA, Org: orgA, Model: opus, Project: app}:                  1,
		{Hour: h.Unix(), Account: acctA, Org: orgA, Model: opus, Project: app, Sidechain: true}: 1,
		{Hour: h.Unix(), Model: opus, Project: app}:                                             1,
	}
	if len(rows) != len(want) {
		t.Fatalf("rows %+v", rows)
	}
	for _, r := range rows {
		if want[r.Key()] != r.Requests {
			t.Errorf("unexpected row %+v", r)
		}
	}
}

func TestSlotAccountsAndLabels(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	hermes := t.TempDir()
	cfg := "plugins:\n  entries:\n    claude-accounts:\n      settings:\n        accounts:\n" +
		"          - id: work\n            label: Claude (work)\n          - id: Bad_Id\n          - id: work\n          - id: spare\n"
	if err := os.WriteFile(filepath.Join(hermes, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	slots := HermesSlots(filepath.Join(hermes, "config.yaml"))
	if len(slots) != 2 || slots[0] != (Slot{"work", "Claude (work)"}) || slots[1] != (Slot{slotSpare, slotSpare}) {
		t.Fatalf("slots %+v", slots)
	}
	if err := os.MkdirAll(filepath.Join(root, slotExtra), 0o700); err != nil {
		t.Fatal(err)
	}
	src := HermesSources(root, slots)
	if len(src) != 3 || src[2].Slot != slotExtra || !src[0].OwnAccount {
		t.Fatalf("sources %+v", src)
	}
	conf := marshal(map[string]any{"oauthAccount": map[string]any{"accountUuid": acctW, "organizationUuid": "org-w",
		"emailAddress": "alice@example.com", "displayName": "Alice"}, "projects": map[string]any{"/home/alice": secret}})
	write(t, filepath.Join(root, "work", ".claude.json"), conf)
	write(t, filepath.Join(root, "work", "projects", "p", "s1.jsonl"), base("s1", "msg_1", "r1", now()).json())
	c := newCollector(t, "", src...)
	scan(t, c)
	rep := c.Report()
	if len(rep.Rows) != 1 || rep.Rows[0].Account != acctW {
		t.Fatalf("rows %+v", rep.Rows)
	}
	want := []Account{{UUID: acctW, Org: "org-w", Label: "Claude (work)", Slot: "work"}, {Label: slotSpare, Slot: slotSpare},
		{Label: slotExtra, Slot: slotExtra}}
	if !slices.Equal(rep.Accounts, want) {
		t.Fatalf("accounts %+v", rep.Accounts)
	}
}

func TestStateSurvivesRestartWithoutRecounting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state := filepath.Join(t.TempDir(), "state.gob")
	path := filepath.Join(dir, "projects", "p", "s1.jsonl")
	write(t, path, ownerLine("s1", acctA), base("s1", "msg_1", "r1", now()).json())
	mk := func() *Collector {
		return New(Options{Sources: []Source{{Dir: dir}}, StateFile: state, ReadRate: 1 << 40, Now: now})
	}
	c := mk()
	scan(t, c)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(state)
	if bytes.Contains(data, []byte(secret)) {
		t.Fatal("state file holds message text")
	}
	// A copy of msg_1 arrives while the daemon is down.
	write(t, path, base("s1", "msg_1", "r1", now()).json(), base("s1", "msg_2", "r2", now()).json())
	d := mk()
	d.Load()
	scan(t, d)
	got := total(d.Report())
	if got.req != 2 || d.Report().Rows[0].Account != acctA {
		t.Fatalf("got %+v %+v", got, d.Report().Rows)
	}
	// A reload keeps every response.
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	e := New(Options{Sources: []Source{{Dir: dir}}, StateFile: state, Now: now})
	e.Load()
	if len(e.tab.entries) != 2 {
		t.Fatalf("entries %d", len(e.tab.entries))
	}
}

func TestPruneKeepsAliases(t *testing.T) {
	t.Parallel()
	tab := newTable()
	at := now()
	r := &record{At: at, Session: "s1", MessageID: "msg_1", HasID: true, RequestID: "r1", HasReq: true, Tokens: tokens{Input: 1}}
	tab.add(r, 0)
	cp := *r
	cp.Session = "s2"
	tab.add(&cp, 1)
	replay := cp
	replay.RequestID, replay.Sidechain = "r9", true
	oldEntry := *r
	oldEntry.MessageID, oldEntry.RequestID, oldEntry.At = "msg_0", "r0", at.Add(-48*time.Hour)
	tab.add(&oldEntry, 0)
	tab.prune(at.Add(-time.Hour).UnixMilli())
	if len(tab.entries) != 1 {
		t.Fatalf("entries %d", len(tab.entries))
	}
	if tab.add(&replay, 1) {
		t.Fatal("a sidechain replay in the copied session counted after prune")
	}
}

func TestRunSendsDeltas(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "projects", "p", "s1.jsonl"), base("s1", "msg_1", "r1", now()).json())
	c := newCollector(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan Report, 4)
	go c.Run(ctx, func(_ context.Context, r Report) error {
		got <- r
		return nil
	})
	select {
	case r := <-got:
		if len(r.Rows) != 1 {
			t.Fatalf("first report %+v", r)
		}
		b := marshal(r)
		if strings.Contains(b, secret) || strings.Contains(b, "/home/alice") {
			t.Fatal("report holds message text or a path")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no report")
	}
}

func TestChangedHoursResendsAnHourWhoseRowMoved(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "projects", "p", "s1.jsonl")
	h := now().Truncate(time.Hour)
	write(t, path, base("s1", "msg_1", "r1", h.Add(time.Minute)).json())
	c := newCollector(t, dir)
	scan(t, c)
	rep := c.Report()
	if rep.Since != h.Add(-freshHistory).Unix() {
		t.Fatalf("since %d, want %d", rep.Since, h.Add(-freshHistory).Unix())
	}
	sent := byHour(rep.Rows)
	full := changedHours(nil, sent, rep.Since, now())
	if len(full) != int(freshHistory/time.Hour)+1 || full[len(full)-1] != h.Unix() {
		t.Fatalf("full sync hours %d", len(full))
	}
	write(t, path, ownerLine("s1", acctA))
	scan(t, c)
	rep = c.Report()
	hours := changedHours(sent, byHour(rep.Rows), rep.Since, now())
	if len(hours) != 1 || hours[0] != h.Unix() || len(rep.Rows) != 1 || rep.Rows[0].Account != acctA {
		t.Fatalf("hours %v rows %+v", hours, rep.Rows)
	}
	if got := changedHours(byHour(rep.Rows), map[int64][]Row{}, rep.Since, now()); len(got) != 1 {
		t.Fatalf("an hour whose rows are gone is not sent: %v", got)
	}
}

func TestDeliverKeepsHoursWhole(t *testing.T) {
	t.Parallel()
	c := newCollector(t, t.TempDir())
	cur := map[int64][]Row{}
	hours := make([]int64, 0, 4)
	for h := range int64(3) {
		hours = append(hours, h*3600)
		for i := range batchRows - 1 {
			cur[h*3600] = append(cur[h*3600], Row{Hour: h * 3600, Project: strconv.Itoa(i)})
		}
	}
	hours = append(hours, 99*3600)
	var got []Report
	if err := c.deliver(context.Background(), func(_ context.Context, r Report) error {
		got = append(got, r)
		return nil
	}, hours, cur, nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(got[0].Hours) != 2 || len(got[0].Rows) != 2*(batchRows-1) || len(got[1].Hours) != 2 {
		t.Fatalf("batches %d", len(got))
	}
}

func TestStateFollowsSourcesByDirectory(t *testing.T) {
	t.Parallel()
	a, b := t.TempDir(), t.TempDir()
	state := filepath.Join(t.TempDir(), "state.gob")
	write(t, filepath.Join(a, "projects", "p", "s1.jsonl"), ownerLine("s1", acctA), base("s1", "msg_1", "r1", now()).json())
	write(t, filepath.Join(b, "projects", "p", "s2.jsonl"), ownerLine("s2", acctW), base("s2", "msg_2", "r2", now()).json())
	mk := func(dirs ...string) *Collector {
		src := make([]Source, 0, len(dirs))
		for _, d := range dirs {
			src = append(src, Source{Dir: d})
		}
		return New(Options{Sources: src, StateFile: state, ReadRate: 1 << 40, Now: now})
	}
	c := mk(a, b)
	c.Load()
	if c.Ready() {
		t.Fatal("ready before the first scan")
	}
	scan(t, c)
	if !c.Ready() {
		t.Fatal("not ready after the first scan")
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	// b moves first and a new source comes; a's state is kept, nothing is read again.
	d := mk(t.TempDir(), b, a)
	d.Load()
	if len(d.tab.entries) != 2 || d.complete != c.complete {
		t.Fatalf("entries %d", len(d.tab.entries))
	}
	if changed, _ := d.Scan(context.Background()); changed {
		t.Fatal("a known file counted again")
	}
	if got := total(d.Report()); got.req != 2 {
		t.Fatalf("got %+v", got)
	}
	// a goes away: only b's response stays.
	if err := d.Save(); err != nil {
		t.Fatal(err)
	}
	e := mk(b)
	e.Load()
	if rows := e.Report().Rows; len(rows) != 1 || rows[0].Account != acctW {
		t.Fatalf("rows %+v", rows)
	}
}
