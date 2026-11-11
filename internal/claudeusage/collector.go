package claudeusage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Retention is how far back the collector counts. Older lines are skipped
// and older responses dropped, which bounds memory; the hub keeps what it
// already got.
const Retention = 35 * 24 * time.Hour

// freshHistory is how far back a collector without a state file trusts
// what it reads. Claude Code removes transcripts after cleanupPeriodDays
// (30 by default), so older hours may be missing sessions that the hub
// already counted; they are not sent.
const freshHistory = 28 * 24 * time.Hour

// stateVersion changes when the state file's layout or meaning does; a
// state file of another version is ignored and every file read again.
const stateVersion = 1

// maxLine is the longest transcript line the collector reads. Longer
// lines are skipped.
const maxLine = 64 << 20

// Source is one Claude Code config directory: a CLAUDE_CONFIG_DIR, or
// ~/.claude.
type Source struct {
	// Dir holds projects/ with the transcripts.
	Dir string
	// ConfigFile is the .claude.json that names the signed-in account.
	// Empty: Dir/.claude.json.
	ConfigFile string
	// Label names the account of this directory instead of its email,
	// such as the label of a claude-accounts slot.
	Label string
	// Slot is the claude-accounts slot id of this directory, or "".
	Slot string
	// OwnAccount counts the sessions without a session-owner line as the
	// directory's signed-in account. Right for a claude-accounts slot,
	// whose directory only ever holds one account.
	OwnAccount bool
}

// Options configure a Collector.
type Options struct {
	Sources []Source
	// StateFile keeps offsets and counted responses across restarts.
	// Empty: nothing is kept, and a restart reads every file again.
	StateFile string
	// ReadRate bounds the bytes per second read from transcripts, so the
	// first scan does not hog the disk. 0: 128 MiB/s.
	ReadRate int64
	Logger   *slog.Logger
	Now      func() time.Time
}

type fileState struct {
	Offset int64
	Size   int64
	Ino    uint64
	Dev    uint64
}

type dimKey struct {
	Hour      int64
	Session   string
	Model     string
	Project   string
	Sidechain bool
	Source    int
}

type sessionOwner struct {
	Account, Org string
}

// Collector reads the transcripts of its sources incrementally. It is
// safe for concurrent use.
type Collector struct {
	o   Options
	log *slog.Logger

	mu     sync.Mutex
	files  map[string]fileState
	dims   []dimKey
	dimIdx map[dimKey]uint32
	owners map[string]sessionOwner
	tab    *table
	pruned time.Time
	// complete is the first hour whose rows are complete: the start of
	// counting minus freshHistory, for a collector that started without a
	// state file.
	complete time.Time
	changed  bool
	loaded   bool
	scanned  bool
	labels   map[string]cachedConfig
}

// New returns a collector. Load restores its state.
func New(o Options) *Collector {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.ReadRate <= 0 {
		o.ReadRate = 128 << 20
	}
	c := &Collector{o: o, log: o.Logger.With("component", "claude-usage"), labels: map[string]cachedConfig{}}
	c.reset()
	return c
}

func (c *Collector) reset() {
	c.complete = c.o.Now().Add(-freshHistory).Truncate(time.Hour)
	c.files = map[string]fileState{}
	c.dims = nil
	c.dimIdx = map[dimKey]uint32{}
	c.owners = map[string]sessionOwner{}
	c.tab = newTable()
}

func (c *Collector) cutoff() time.Time { return c.o.Now().Add(-Retention) }

func (c *Collector) dim(k dimKey) uint32 {
	if i, ok := c.dimIdx[k]; ok {
		return i
	}
	i := uint32(len(c.dims)) //nolint:gosec // far fewer dims than 2^32
	c.dims = append(c.dims, k)
	c.dimIdx[k] = i
	return i
}

// Scan reads what was appended to every transcript since the last scan.
// It reports whether anything was counted.
func (c *Collector) Scan(ctx context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := false
	seen := map[string]bool{}
	cutoff := c.cutoff()
	for si, src := range c.o.Sources {
		root := filepath.Join(src.Dir, "projects")
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
					return filepath.SkipDir
				}
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			seen[path] = true
			ch, err := c.scanFile(ctx, si, path, cutoff)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				c.log.Debug("Could not read a transcript", "error", err)
				return nil
			}
			changed = changed || ch
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return changed, err
		}
	}
	for p := range c.files {
		if !seen[p] {
			delete(c.files, p)
		}
	}
	if c.o.Now().Sub(c.pruned) > 24*time.Hour {
		c.tab.prune(cutoff.UnixMilli())
		c.pruned = c.o.Now()
	}
	c.changed = c.changed || changed
	c.scanned = true
	return changed, nil
}

// Ready reports whether the collector finished its first scan, so that
// its report holds everything it will count.
func (c *Collector) Ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scanned
}

func (c *Collector) scanFile(ctx context.Context, si int, path string, cutoff time.Time) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	ino, dev := fileID(fi)
	st, known := c.files[path]
	switch {
	case !known && fi.ModTime().Before(cutoff):
		c.files[path] = fileState{Offset: fi.Size(), Size: fi.Size(), Ino: ino, Dev: dev}
		return false, nil
	case known && (st.Ino != ino || st.Dev != dev || fi.Size() < st.Offset):
		st = fileState{}
	case known && fi.Size() == st.Offset:
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		return false, err
	}
	sess := fileSession(path)
	cutMS := cutoff.UnixMilli()
	r := bufio.NewReaderSize(io.LimitReader(f, fi.Size()-st.Offset), 1<<20)
	offset := st.Offset
	changed := false
	start := time.Now()
	var read int64
	var long []byte
	var pending int64
	skip := false
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			pending += int64(len(line))
			if !skip && len(long)+len(line) <= maxLine {
				long = append(long, line...)
			} else {
				skip, long = true, nil
			}
			continue
		}
		if err != nil {
			break
		}
		n := pending + int64(len(line))
		pending = 0
		if long != nil {
			line = append(long, line...)
			long = nil
		}
		offset += n
		read += n
		if skip {
			skip = false
			continue
		}
		if c.line(si, bytes.TrimRight(line, "\r\n"), sess, cutMS) {
			changed = true
		}
		if read >= 8<<20 {
			if err := c.throttle(ctx, read, start); err != nil {
				c.files[path] = fileState{Offset: offset, Size: fi.Size(), Ino: ino, Dev: dev}
				return changed, err
			}
			read, start = 0, time.Now()
		}
	}
	c.files[path] = fileState{Offset: offset, Size: fi.Size(), Ino: ino, Dev: dev}
	return changed, nil
}

// throttle sleeps so that reading n bytes since start keeps to ReadRate.
func (c *Collector) throttle(ctx context.Context, n int64, start time.Time) error {
	want := time.Duration(float64(n) / float64(c.o.ReadRate) * float64(time.Second))
	if d := want - time.Since(start); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return ctx.Err()
}

func (c *Collector) line(si int, line []byte, sess string, cutMS int64) bool {
	recs, own := parseLine(line, sess)
	if own != nil {
		prev, ok := c.owners[own.Session]
		next := sessionOwner{Account: own.Account, Org: own.Org}
		c.owners[own.Session] = next
		return !ok || prev != next
	}
	changed := false
	for i := range recs {
		r := &recs[i]
		if r.At.UnixMilli() < cutMS {
			continue
		}
		d := c.dim(dimKey{Hour: r.At.Truncate(time.Hour).Unix(), Session: r.Session, Model: r.Model, Project: r.Project,
			Sidechain: r.Sidechain, Source: si})
		if c.tab.add(r, d) {
			changed = true
		}
	}
	return changed
}

// Row is the tokens of one hour, account, organization, model, project
// and thread kind (docs/protocol.md section 7).
type Row struct {
	Hour         int64  `json:"hour"`
	Account      string `json:"account_uuid"`
	Org          string `json:"org_uuid"`
	Model        string `json:"model"`
	Project      string `json:"project"`
	Sidechain    bool   `json:"sidechain"`
	Input        uint64 `json:"input"`
	Output       uint64 `json:"output"`
	CacheWrite   uint64 `json:"cache_write"`
	CacheWrite1h uint64 `json:"cache_write_1h"`
	CacheRead    uint64 `json:"cache_read"`
	Requests     uint64 `json:"requests"`
}

// RowKey is what identifies a Row.
type RowKey struct {
	Hour      int64
	Account   string
	Org       string
	Model     string
	Project   string
	Sidechain bool
}

// Key returns the key of r.
func (r Row) Key() RowKey {
	return RowKey{Hour: r.Hour, Account: r.Account, Org: r.Org, Model: r.Model, Project: r.Project, Sidechain: r.Sidechain}
}

// Account names an account that a source is signed in as, or a
// claude-accounts slot that is not signed in (empty UUID).
type Account struct {
	UUID  string `json:"account_uuid"`
	Org   string `json:"org_uuid"`
	Label string `json:"label"`
	Slot  string `json:"slot,omitempty"`
}

// Report is what the hub gets. Its rows replace the hub's rows of the
// same source for each hour in Hours and every hour from Since on, so a
// row that is gone, such as one whose session got its owner later, goes
// away on the hub too. Accounts are the accounts the sources are signed
// in as.
type Report struct {
	Since    int64     `json:"since,omitempty"`
	Hours    []int64   `json:"hours,omitempty"`
	Rows     []Row     `json:"rows"`
	Accounts []Account `json:"accounts"`
}

// since is the first hour a report covers: the retention cutoff, or the
// first complete hour when that is later.
func (c *Collector) since() time.Time {
	s := c.cutoff().Truncate(time.Hour).Add(time.Hour)
	if c.complete.After(s) {
		s = c.complete
	}
	return s
}

// Report sums the counted responses into rows: all rows from Since on.
func (c *Collector) Report() Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	accounts, own := c.accounts()
	since := c.since()
	cutMS := since.UnixMilli()
	rows := map[RowKey]*Row{}
	for i := range c.tab.entries {
		e := &c.tab.entries[i]
		if e.AtMS < cutMS {
			continue
		}
		d := c.dims[e.Dims]
		o, ok := c.owners[d.Session]
		if !ok && d.Source < len(own) {
			o = own[d.Source]
		}
		k := RowKey{Hour: d.Hour, Account: o.Account, Org: o.Org, Model: d.Model, Project: d.Project, Sidechain: d.Sidechain}
		r := rows[k]
		if r == nil {
			r = &Row{Hour: k.Hour, Account: k.Account, Org: k.Org, Model: k.Model, Project: k.Project, Sidechain: k.Sidechain}
			rows[k] = r
		}
		t := e.tokens()
		r.Input += t.Input
		r.Output += t.Output
		r.CacheWrite += t.CacheWrite
		r.CacheWrite1h += t.CacheWrite1h
		r.CacheRead += t.CacheRead
		r.Requests++
	}
	out := Report{Since: since.Unix(), Rows: make([]Row, 0, len(rows)), Accounts: accounts}
	for _, r := range rows {
		out.Rows = append(out.Rows, *r)
	}
	sort.Slice(out.Rows, func(i, j int) bool { return lessKey(out.Rows[i].Key(), out.Rows[j].Key()) })
	return out
}

func lessKey(a, b RowKey) bool {
	switch {
	case a.Hour != b.Hour:
		return a.Hour < b.Hour
	case a.Account != b.Account:
		return a.Account < b.Account
	case a.Org != b.Org:
		return a.Org < b.Org
	case a.Model != b.Model:
		return a.Model < b.Model
	case a.Project != b.Project:
		return a.Project < b.Project
	}
	return !a.Sidechain && b.Sidechain
}

// accounts reads each source's signed-in account. own holds, by source,
// the account that sessions without an owner count as.
func (c *Collector) accounts() ([]Account, []sessionOwner) {
	own := make([]sessionOwner, len(c.o.Sources))
	out := []Account{}
	seen := map[string]bool{}
	for i, src := range c.o.Sources {
		a := c.readConfig(src)
		if src.OwnAccount {
			own[i] = sessionOwner{Account: a.UUID, Org: a.Org}
		}
		if src.Label != "" {
			a.Label = src.Label
		}
		a.Slot = src.Slot
		if a.UUID == "" && a.Slot == "" {
			continue
		}
		k := a.UUID + "\x00" + a.Slot
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, a)
	}
	return out, own
}

type cachedConfig struct {
	mod  time.Time
	size int64
	acct Account
}

// readConfig reads oauthAccount from a source's .claude.json: the account
// uuid, organization uuid and email, and nothing else.
func (c *Collector) readConfig(src Source) Account {
	path := src.ConfigFile
	if path == "" {
		path = filepath.Join(src.Dir, ".claude.json")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return Account{}
	}
	if cc, ok := c.labels[path]; ok && cc.mod.Equal(fi.ModTime()) && cc.size == fi.Size() {
		return cc.acct
	}
	a := readOAuthAccount(path)
	c.labels[path] = cachedConfig{mod: fi.ModTime(), size: fi.Size(), acct: a}
	return a
}

// state is the layout of the state file.
type state struct {
	Version  int
	Sources  []string
	Files    map[string]fileState
	Dims     []dimKey
	Owners   map[string]sessionOwner
	Entries  []entry
	Aliases  []alias
	Pruned   time.Time
	Complete time.Time
}

func (c *Collector) sourceDirs() []string {
	out := make([]string, len(c.o.Sources))
	for i, s := range c.o.Sources {
		out[i] = s.Dir
	}
	return out
}

// Load restores the state file. A missing, unreadable or outdated one
// starts over.
func (c *Collector) Load() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded {
		return
	}
	c.loaded = true
	if c.o.StateFile == "" {
		return
	}
	f, err := os.Open(c.o.StateFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.log.Warn("Could not open the Claude usage state; reading every transcript again", "error", err)
		}
		return
	}
	defer f.Close()
	var st state
	if err := gob.NewDecoder(bufio.NewReader(f)).Decode(&st); err != nil || st.Version != stateVersion {
		c.log.Info("Claude usage state is outdated or unreadable; reading every transcript again")
		return
	}
	// Sources are matched by directory, so a source added, removed or
	// moved keeps the others' state. What a removed source counted goes.
	remap := map[int]int{}
	for i, d := range st.Sources {
		if j := slices.Index(c.sourceDirs(), d); j >= 0 {
			remap[i] = j
		}
	}
	dropped := map[uint32]bool{}
	for i := range st.Dims {
		if j, ok := remap[st.Dims[i].Source]; ok {
			st.Dims[i].Source = j
		} else {
			dropped[uint32(i)] = true //nolint:gosec // far fewer dims than 2^32
		}
	}
	c.files, c.dims, c.owners = st.Files, st.Dims, st.Owners
	if c.files == nil {
		c.files = map[string]fileState{}
	}
	if c.owners == nil {
		c.owners = map[string]sessionOwner{}
	}
	c.dimIdx = make(map[dimKey]uint32, len(c.dims))
	for i, d := range c.dims {
		c.dimIdx[d] = uint32(i)
	}
	c.tab = &table{entries: st.Entries, aliases: st.Aliases}
	if len(dropped) > 0 {
		c.tab.filter(func(e *entry) bool { return !dropped[e.Dims] })
		c.compactDims()
		c.changed = true
	} else {
		c.tab.reindex()
	}
	c.pruned, c.complete = st.Pruned, st.Complete
}

// Save writes the state file when something changed since the last save.
func (c *Collector) Save() error {
	if c.o.StateFile == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.changed {
		return nil
	}
	c.compactDims()
	st := state{Version: stateVersion, Sources: c.sourceDirs(), Files: c.files, Dims: c.dims, Owners: c.owners,
		Entries: c.tab.entries, Aliases: c.tab.aliases, Pruned: c.pruned,
		Complete: c.complete}
	if err := os.MkdirAll(filepath.Dir(c.o.StateFile), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.o.StateFile), ".claude-usage-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	w := bufio.NewWriterSize(tmp, 1<<20)
	if err := gob.NewEncoder(w).Encode(&st); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), c.o.StateFile); err != nil {
		return fmt.Errorf("save Claude usage state: %w", err)
	}
	c.changed = false
	return nil
}

// compactDims drops the dims no entry uses any more. Session owners stay:
// a session-owner line often comes before the session's first response.
func (c *Collector) compactDims() {
	used := make([]bool, len(c.dims))
	for i := range c.tab.entries {
		used[c.tab.entries[i].Dims] = true
	}
	remap := make([]uint32, len(c.dims))
	var dims []dimKey
	for i, d := range c.dims {
		if used[i] {
			remap[i] = uint32(len(dims)) //nolint:gosec // far fewer dims than 2^32
			dims = append(dims, d)
		}
	}
	if len(dims) == len(c.dims) {
		return
	}
	for i := range c.tab.entries {
		c.tab.entries[i].Dims = remap[c.tab.entries[i].Dims]
	}
	c.dims = dims
	c.dimIdx = make(map[dimKey]uint32, len(dims))
	for i, d := range dims {
		c.dimIdx[d] = uint32(i)
	}
}
