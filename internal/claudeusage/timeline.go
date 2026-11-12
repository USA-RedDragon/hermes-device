package claudeusage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// WatchInterval is how often Run looks at each .claude.json for a switch
// of the signed-in account. A look is a stat; the file is read only when
// it changed.
const WatchInterval = 2 * time.Second

// timelineName is the default file of the account timeline, next to the
// state file.
const timelineName = "claude-accounts.jsonl"

// maxTimelineLine bounds one line of the timeline file.
const maxTimelineLine = 1 << 16

// switchEntry is one line of the timeline: from AtMS on, a source's
// .claude.json named this account. Only ids and the email are kept, never
// a token.
type switchEntry struct {
	AtMS    int64  `json:"at_ms"`
	Dir     string `json:"dir"`
	Account string `json:"account_uuid"`
	Org     string `json:"org_uuid"`
	Email   string `json:"email,omitempty"`
}

type configStat struct {
	mod  time.Time
	size int64
}

// timeline is the append-only record of which account each source was
// signed in as, and since when. Claude Code does not write the account
// into transcript lines, and a running session follows a login at once,
// so the only way to tell which account a request used is the account
// that was signed in at its time.
type timeline struct {
	mu    sync.Mutex
	path  string
	log   *slog.Logger
	byDir map[string][]switchEntry
	seen  map[string]configStat
}

func newTimeline(path string, log *slog.Logger) *timeline {
	t := &timeline{path: path, log: log, byDir: map[string][]switchEntry{}, seen: map[string]configStat{}}
	t.load()
	return t
}

func (t *timeline) load() {
	if t.path == "" {
		return
	}
	f, err := os.Open(t.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			t.log.Warn("Could not open the Claude account timeline", "error", err)
		}
		return
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = r.ReadSlice('\n')
			}
			continue
		}
		if len(line) > 0 && len(line) <= maxTimelineLine {
			var e switchEntry
			if json.Unmarshal(line, &e) == nil && e.Account != "" && e.Dir != "" {
				t.byDir[e.Dir] = append(t.byDir[e.Dir], e)
			}
		}
		if err != nil {
			break
		}
	}
	for _, s := range t.byDir {
		sort.SliceStable(s, func(i, j int) bool { return s[i].AtMS < s[j].AtMS })
	}
}

// observe looks at the .claude.json of every source that is not a
// claude-accounts slot and appends an entry for each one whose signed-in
// account changed. The first entry of a source starts at now; later ones
// at the file's modification time, kept between the previous entry and
// now. An unreadable file or one without an account adds nothing.
func (t *timeline) observe(sources []Source, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := false
	for _, src := range sources {
		if src.OwnAccount {
			continue
		}
		path := configPath(src)
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if s, ok := t.seen[src.Dir]; ok && s.mod.Equal(fi.ModTime()) && s.size == fi.Size() {
			continue
		}
		a := readOAuthAccount(path)
		t.seen[src.Dir] = configStat{mod: fi.ModTime(), size: fi.Size()}
		if a.UUID == "" {
			continue
		}
		list := t.byDir[src.Dir]
		at := now
		if n := len(list); n > 0 {
			last := list[n-1]
			if last.Account == a.UUID && last.Org == a.Org {
				continue
			}
			at = fi.ModTime()
			if at.After(now) {
				at = now
			}
			if at.UnixMilli() < last.AtMS {
				at = time.UnixMilli(last.AtMS)
			}
		}
		e := switchEntry{AtMS: at.UnixMilli(), Dir: src.Dir, Account: a.UUID, Org: a.Org, Email: a.Label}
		t.append(e)
		t.byDir[src.Dir] = append(list, e)
		changed = true
	}
	return changed
}

func (t *timeline) append(e switchEntry) {
	if t.path == "" {
		return
	}
	b, err := json.Marshal(e)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(t.path), 0o700)
	}
	var f *os.File
	if err == nil {
		f, err = os.OpenFile(t.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	}
	if err == nil {
		_, err = f.Write(append(b, '\n'))
		if cerr := f.Sync(); err == nil {
			err = cerr
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		t.log.Warn("Could not append to the Claude account timeline", "error", err)
	}
}

// snapshot returns the entries by source. Entries are only ever appended,
// so the slices stay valid while the timeline grows.
func (t *timeline) snapshot() map[string][]switchEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string][]switchEntry, len(t.byDir))
	for k, v := range t.byDir {
		out[k] = v[:len(v):len(v)]
	}
	return out
}

// at is the entry in force at atMS: the last one at or before it.
func at(list []switchEntry, atMS int64) (switchEntry, bool) {
	i := sort.Search(len(list), func(i int) bool { return list[i].AtMS > atMS })
	if i == 0 {
		return switchEntry{}, false
	}
	return list[i-1], true
}

func configPath(src Source) string {
	if src.ConfigFile != "" {
		return src.ConfigFile
	}
	return filepath.Join(src.Dir, ".claude.json")
}

// Observe records a switch of the signed-in account of any source now.
// Run calls it every WatchInterval.
func (c *Collector) Observe() bool {
	return c.tl.observe(c.o.Sources, c.o.Now())
}

func (c *Collector) watchAccounts(ctx context.Context) {
	t := time.NewTicker(WatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Observe()
		}
	}
}
