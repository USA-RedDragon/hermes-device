// Package updates checks Talaria's public releases once a day, read only,
// and says which parts of this install are behind: the hub, the agent
// image and paired computers. Only admins see the answer.
package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// DefaultFeed is the GitHub releases list of Talaria's repository.
const DefaultFeed = "https://api.github.com/repos/USA-RedDragon/talaria/releases?per_page=100"

// Sources tell the checker what this install runs.
type Sources struct {
	// Agents returns the agent release (upstream version) each backend
	// reports, by backend name.
	Agents func(ctx context.Context) map[string]string
	// Computers returns each paired computer's name and talaria version.
	Computers func(ctx context.Context) [][2]string
}

// Checker caches the newest releases.
type Checker struct {
	feed    string
	client  *http.Client
	current string
	install string
	src     Sources
	every   time.Duration

	mu      sync.Mutex
	rels    []release
	checked time.Time
	log     *slog.Logger
}

type release struct {
	Tag        string    `json:"tag_name"`
	URL        string    `json:"html_url"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published_at"`
}

// New returns a Checker. feed "" means DefaultFeed.
func New(feed, current, install string, src Sources, client *http.Client) *Checker {
	if feed == "" {
		feed = DefaultFeed
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Checker{feed: feed, client: client, current: current, install: install, src: src, every: 24 * time.Hour, log: slog.Default()}
}

// Run checks at start and then once a day until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	for {
		if err := c.Refresh(ctx); err != nil && ctx.Err() == nil {
			c.log.Info("Could not check for Talaria updates", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.every):
		}
	}
}

// Refresh reads the releases feed now.
func (c *Checker) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.feed, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("releases feed answered %d", resp.StatusCode)
	}
	var rels []release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rels); err != nil {
		return err
	}
	c.mu.Lock()
	c.rels, c.checked = rels, time.Now()
	c.mu.Unlock()
	return nil
}

// newest is the newest release whose tag starts with prefix (and is not a
// tag of another part, for the bare daemon "v").
func newest(rels []release, prefix string) (release, []int, bool) {
	var best release
	var bestV []int
	found := false
	for _, r := range rels {
		if r.Draft || r.Prerelease || !strings.HasPrefix(r.Tag, prefix) {
			continue
		}
		v, ok := Parse(strings.TrimPrefix(r.Tag, prefix))
		if !ok {
			continue
		}
		if !found || Less(bestV, v) {
			best, bestV, found = r, v, true
		}
	}
	return best, bestV, found
}

// Parse reads a version such as 0.26.70, v0.26.70 or 0.23.1-p34 into
// numbers; a -pN suffix is the last number.
func Parse(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return nil, false
	}
	main, patch, hasPatch := strings.Cut(s, "-p")
	if i := strings.IndexAny(main, "-+"); i >= 0 {
		main = main[:i]
	}
	var out []int
	for _, part := range strings.Split(main, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	if hasPatch {
		n, err := strconv.Atoi(patch)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// Less reports a < b.
func Less(a, b []int) bool {
	for i := range max(len(a), len(b)) {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// Status says what is behind, from the last check.
func (c *Checker) Status(ctx context.Context) hub.UpdateStatus {
	c.mu.Lock()
	rels, checked := c.rels, c.checked
	c.mu.Unlock()
	st := hub.UpdateStatus{Current: c.current, Install: c.install, CheckedAt: checked}
	if len(rels) == 0 {
		return st
	}
	if r, v, ok := newest(rels, "hub/v"); ok {
		if cur, ok := Parse(c.current); ok && Less(cur, v) {
			st.Latest = &hub.Release{Version: strings.TrimPrefix(r.Tag, "hub/v"), URL: r.URL, At: r.Published}
		}
	}
	if r, v, ok := newest(rels, "hermes/v"); ok && c.src.Agents != nil {
		latest := strings.TrimPrefix(r.Tag, "hermes/v")
		base := v
		if strings.Contains(latest, "-p") {
			base = v[:len(v)-1]
		}
		for name, running := range c.src.Agents(ctx) {
			cur, ok := Parse(running)
			if ok && Less(cur, base) {
				st.Agent = append(st.Agent, hub.AgentVersion{Backend: name, Running: strings.TrimPrefix(running, "v"), Latest: latest})
			}
		}
	}
	if _, v, ok := newest(rels, "v"); ok && c.src.Computers != nil {
		latest := versionString(v)
		for _, d := range c.src.Computers(ctx) {
			cur, ok := Parse(d[1])
			if ok && Less(cur, v) {
				st.Computers = append(st.Computers, hub.ComputerVersion{Name: d[0], Version: strings.TrimPrefix(d[1], "v"), Latest: latest})
			}
		}
	}
	return st
}

func versionString(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ".")
}
