// Package claudecode keeps the Claude Code token counts that talaria sends
// from each computer and from each Hermes pod's acp-host, and serves the
// Usage page's Claude Code panel: totals, tokens per day and a table by
// account, computer, model or project, priced at API rates.
package claudecode

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// Ranges and groups of the panel.
const (
	RangeToday = "today"
	Range7d    = "7d"
	Range30d   = "30d"
	RangeMTD   = "mtd"
	Range90d   = "90d"

	GroupAccount  = "account"
	GroupComputer = "computer"
	GroupModel    = "model"
	GroupProject  = "project"
)

// keptDays is about how far back computers keep Claude Code transcripts,
// so the first day a longer range can have counts for.
const keptDays = 35

// Limits of a report.
const (
	maxRows   = 50000
	maxHours  = 5000
	maxString = 256
	// maxAge is how old an hour may be; older rows are dropped.
	maxAge = 62 * 24 * time.Hour
)

// Timing.
const (
	pollInterval = 5 * time.Minute
	// freshAfter is how old a Hermes read may be before the panel asks
	// again, and pollWait how long it waits for that.
	freshAfter = time.Minute
	pollWait   = 4 * time.Second
)

// Access says whether a user may use a backend. *service.Service is one.
type Access interface {
	Allowed(ctx context.Context, user, backendID string) error
}

// Devices finds a computer by id, for its current name.
type Devices interface {
	Device(ctx context.Context, id string) (hub.Device, bool, error)
}

// Backend is a backend whose Hermes pod may report usage.
type Backend struct {
	ID, Name string
	// Hermes is true when its acp-host can be asked for usage.
	Hermes bool
}

// Fetcher reads the usage report of a backend's acp-host.
type Fetcher func(ctx context.Context, backendID string) ([]byte, error)

// Options are the dependencies of a Service.
type Options struct {
	Store    hub.ClaudeCodeStore
	Access   Access
	Devices  Devices
	Backends []Backend
	Fetch    Fetcher
	Prices   *Prices
	// Location is the time zone of days. Default UTC.
	Location *time.Location
	Logger   *slog.Logger
	Now      func() time.Time
}

// Service implements hub.ClaudeCode.
type Service struct {
	o   Options
	log *slog.Logger

	mu      sync.Mutex
	polled  map[string]time.Time
	polling map[string]chan struct{}
}

var _ hub.ClaudeCode = (*Service)(nil)

// New returns a Service.
func New(o Options) (*Service, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Prices == nil {
		p, err := DefaultPrices()
		if err != nil {
			return nil, err
		}
		o.Prices = p
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	return &Service{o: o, log: o.Logger.With("component", "claude-code"), polled: map[string]time.Time{},
		polling: map[string]chan struct{}{}}, nil
}

func deviceSource(id string) string { return "device:" + id }
func hermesSource(id string) string { return "hermes:" + id }
func validString(s string) bool     { return len(s) <= maxString && utf8.ValidString(s) }
func nonNegative(v ...int64) bool {
	return !slices.ContainsFunc(v, func(x int64) bool { return x < 0 })
}
func (s *Service) oldest() int64       { return s.o.Now().Add(-maxAge).Unix() }
func hourAligned(h int64) bool         { return h%3600 == 0 }
func (s *Service) future(h int64) bool { return h > s.o.Now().Add(48*time.Hour).Unix() }

// decode reads and checks a report, and drops rows older than maxAge.
func (s *Service) decode(body []byte) (hub.ClaudeCodeReport, error) {
	var r hub.ClaudeCodeReport
	if err := json.Unmarshal(body, &r); err != nil {
		return r, fmt.Errorf("%w: %w", hub.ErrBadClaudeCodeReport, err)
	}
	if len(r.Rows) > maxRows || len(r.Hours) > maxHours || len(r.Accounts) > 100 {
		return r, fmt.Errorf("%w: too large", hub.ErrBadClaudeCodeReport)
	}
	if r.Since != 0 && (!hourAligned(r.Since) || s.future(r.Since)) {
		return r, fmt.Errorf("%w: since", hub.ErrBadClaudeCodeReport)
	}
	if r.Since != 0 && r.Since < s.oldest() {
		r.Since = s.oldest() - s.oldest()%3600
	}
	for _, h := range r.Hours {
		if !hourAligned(h) || s.future(h) {
			return r, fmt.Errorf("%w: hours", hub.ErrBadClaudeCodeReport)
		}
	}
	rows := r.Rows[:0]
	for _, x := range r.Rows {
		if !hourAligned(x.Hour) || s.future(x.Hour) || !nonNegative(x.Input, x.Output, x.CacheWrite, x.CacheWrite1h, x.CacheRead, x.Requests) ||
			x.CacheWrite1h > x.CacheWrite || !validString(x.Account) || !validString(x.Org) || !validString(x.Model) || !validString(x.Project) {
			return r, fmt.Errorf("%w: row", hub.ErrBadClaudeCodeReport)
		}
		if x.Hour >= s.oldest() {
			rows = append(rows, x)
		}
	}
	r.Rows = rows
	for _, a := range r.Accounts {
		if !validString(a.UUID) || !validString(a.Org) || !validString(a.Label) || !validString(a.Slot) {
			return r, fmt.Errorf("%w: account", hub.ErrBadClaudeCodeReport)
		}
	}
	return r, nil
}

// IngestDevice stores a report that computer d sent.
func (s *Service) IngestDevice(ctx context.Context, d hub.Device, body []byte) error {
	r, err := s.decode(body)
	if err != nil {
		return err
	}
	return s.o.Store.PutClaudeCodeReport(ctx, hub.ClaudeCodeSource{ID: deviceSource(d.ID), Kind: hub.ClaudeCodeDevice, User: d.User,
		BackendID: d.BackendID}, r)
}

// Run asks every Hermes for its usage every five minutes until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		for _, b := range s.o.Backends {
			if b.Hermes {
				s.poll(ctx, b.ID)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// poll reads one Hermes's report, once at a time per backend.
func (s *Service) poll(ctx context.Context, backendID string) {
	s.mu.Lock()
	if ch, ok := s.polling[backendID]; ok {
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		return
	}
	ch := make(chan struct{})
	s.polling[backendID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.polling, backendID)
		s.mu.Unlock()
		close(ch)
	}()
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err := s.pollOnce(pctx, backendID)
	s.mu.Lock()
	s.polled[backendID] = s.o.Now()
	s.mu.Unlock()
	if err != nil {
		s.log.Warn("Could not read Claude Code usage from Hermes", "backend", backendID, "error", err.Error())
	}
}

func (s *Service) pollOnce(ctx context.Context, backendID string) error {
	if s.o.Fetch == nil {
		return nil
	}
	body, err := s.o.Fetch(ctx, backendID)
	if err != nil {
		return err
	}
	r, err := s.decode(body)
	if err != nil {
		return err
	}
	if r.Since == 0 {
		r.Since = s.oldest() - s.oldest()%3600
	}
	return s.o.Store.PutClaudeCodeReport(ctx, hub.ClaudeCodeSource{ID: hermesSource(backendID), Kind: hub.ClaudeCodeHermes,
		BackendID: backendID}, r)
}

// refresh reads the Hermes of backends again when the last read is older
// than freshAfter, waiting at most pollWait.
func (s *Service) refresh(ctx context.Context, backends []string) {
	var wg sync.WaitGroup
	for _, id := range backends {
		s.mu.Lock()
		stale := s.o.Now().Sub(s.polled[id]) > freshAfter
		s.mu.Unlock()
		if !stale {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.poll(context.WithoutCancel(ctx), id)
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	t := time.NewTimer(pollWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-ctx.Done():
	}
}

// days returns the first hour and the end hour of rng, and its days.
func (s *Service) days(rng string) (from, to int64, days []time.Time, err error) {
	now := s.o.Now().In(s.o.Location)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.o.Location)
	var n int
	switch rng {
	case RangeToday:
		n = 1
	case Range7d:
		n = 7
	case Range30d:
		n = 30
	case RangeMTD:
		n = now.Day()
	case Range90d:
		n = 90
	default:
		return 0, 0, nil, hub.ErrBadRange
	}
	for i := n - 1; i >= 0; i-- {
		days = append(days, today.AddDate(0, 0, -i))
	}
	return days[0].Unix(), today.AddDate(0, 0, 1).Unix(), days, nil
}

type groupAcc struct {
	g     hub.ClaudeCodeGroup
	order int
}

// tally sums rows into groups over days.
type tally struct {
	days   []time.Time
	loc    *time.Location
	groups map[string]*groupAcc
}

func (t *tally) get(key, label, acct string) *groupAcc {
	g := t.groups[key]
	if g == nil {
		g = &groupAcc{g: hub.ClaudeCodeGroup{Key: key, Label: label, AccountUUID: acct, Daily: make([]int64, len(t.days))},
			order: len(t.groups)}
		t.groups[key] = g
	}
	return g
}

func (t *tally) day(hour int64) int {
	x := time.Unix(hour, 0).In(t.loc)
	d := time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, t.loc)
	return int(d.Sub(t.days[0]).Hours()/24 + 0.5)
}

func (t *tally) sorted() []hub.ClaudeCodeGroup {
	list := make([]*groupAcc, 0, len(t.groups))
	for _, g := range t.groups {
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].g.Total != list[j].g.Total {
			return list[i].g.Total > list[j].g.Total
		}
		return list[i].order < list[j].order
	})
	out := make([]hub.ClaudeCodeGroup, 0, len(list))
	for _, g := range list {
		out = append(out, g.g)
	}
	return out
}

// visible returns the sources user may see, the user's computers and the
// Hermes of each backend they may use, with the name of each.
func (s *Service) visible(ctx context.Context, user string) ([]string, map[string]string, error) {
	var hermes []string
	backendName := map[string]string{}
	for _, b := range s.o.Backends {
		if b.Hermes && s.o.Access.Allowed(ctx, user, b.ID) == nil {
			hermes = append(hermes, b.ID)
			backendName[b.ID] = b.Name
		}
	}
	s.refresh(ctx, hermes)
	all, err := s.o.Store.ClaudeCodeSources(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ids []string
	names := map[string]string{}
	var herm []hub.ClaudeCodeSource
	for _, src := range all {
		switch {
		case src.Kind == hub.ClaudeCodeDevice && src.User == user:
			// A removed computer no longer counts: paired again, it
			// sends the same hours under its new id.
			d, ok, err := s.o.Devices.Device(ctx, strings.TrimPrefix(src.ID, "device:"))
			if err != nil {
				return nil, nil, err
			}
			if !ok || d.User != user {
				continue
			}
			names[src.ID] = d.Name
		case src.Kind == hub.ClaudeCodeHermes && slices.Contains(hermes, src.BackendID):
			names[src.ID] = "Hermes"
			herm = append(herm, src)
		default:
			continue
		}
		ids = append(ids, src.ID)
	}
	if len(herm) > 1 {
		for _, src := range herm {
			names[src.ID] = "Hermes (" + backendName[src.BackendID] + ")"
		}
	}
	return ids, names, nil
}

// key is the group of r and its label.
func key(group string, r hub.ClaudeCodeRow, labels, computers map[string]string) (k, label, acct string) {
	switch group {
	case GroupAccount:
		if r.Account == "" {
			return "", "Unknown account", ""
		}
		return r.Account, labels[r.Account], r.Account
	case GroupComputer:
		return r.Source, computers[r.Source], ""
	case GroupModel:
		return r.Model, ModelLabel(r.Model), ""
	}
	p := r.Project
	if p == "" {
		p = "unknown"
	}
	return p, p, ""
}

// ClaudeCodeUsage sums user's Claude Code usage over rng by group: the
// user's own computers and the Hermes of each backend the user may use.
func (s *Service) ClaudeCodeUsage(ctx context.Context, user, rng, group string) (hub.ClaudeCodeUsage, error) {
	from, to, days, err := s.days(rng)
	if err != nil {
		return hub.ClaudeCodeUsage{}, err
	}
	switch group {
	case GroupAccount, GroupComputer, GroupModel, GroupProject:
	default:
		return hub.ClaudeCodeUsage{}, hub.ErrBadRange
	}
	ids, computers, err := s.visible(ctx, user)
	if err != nil {
		return hub.ClaudeCodeUsage{}, err
	}
	out := hub.ClaudeCodeUsage{Range: rng, Group: group, TimeZone: s.o.Location.String(), Groups: []hub.ClaudeCodeGroup{},
		Unpriced: []string{}}
	for _, d := range days {
		out.Days = append(out.Days, d.Format(time.DateOnly))
	}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.o.Store.ClaudeCodeRows(ctx, ids, from, to)
	if err != nil {
		return hub.ClaudeCodeUsage{}, err
	}
	accounts, err := s.o.Store.ClaudeCodeAccounts(ctx, ids)
	if err != nil {
		return hub.ClaudeCodeUsage{}, err
	}
	names, err := s.o.Store.ClaudeAccountNames(ctx, user)
	if err != nil {
		return hub.ClaudeCodeUsage{}, err
	}
	labels := accountLabels(accounts, names)
	t := &tally{days: days, loc: s.o.Location, groups: map[string]*groupAcc{}}
	unpriced := map[string]bool{}
	first := int64(0)
	for _, r := range rows {
		if first == 0 || r.Hour < first {
			first = r.Hour
		}
		g := t.get(key(group, r, labels, computers))
		total := r.Input + r.Output + r.CacheWrite + r.CacheRead
		cost := 0.0
		if p, ok := s.o.Prices.Find(r.Model); ok {
			cost = p.Cost(r)
		} else if total > 0 {
			unpriced[cmp.Or(r.Model, "unknown")] = true
		}
		add(&g.g.ClaudeCodeTotals, r, total, cost)
		add(&out.Total, r, total, cost)
		if i := t.day(r.Hour); i >= 0 && i < len(days) {
			g.g.Daily[i] += total
		}
	}
	if group == GroupAccount {
		for _, a := range accounts {
			switch {
			case !strings.HasPrefix(a.Source, "hermes:"):
			case a.UUID != "":
				t.get(a.UUID, labels[a.UUID], a.UUID)
			case a.Slot != "" && a.Label == a.Slot:
				// A folder that no slot of the plugin names (acp-host labels it
				// with its folder name) and that never signed in: an old slot.
			case a.Slot != "":
				t.get("slot:"+a.Source+":"+a.Slot, a.Label, "")
			}
		}
	}
	if len(days) > keptDays && first != 0 {
		if d := time.Unix(first, 0).In(s.o.Location).Format(time.DateOnly); d > out.Days[0] {
			out.Since = d
		}
	}
	out.Groups = t.sorted()
	for m := range unpriced {
		out.Unpriced = append(out.Unpriced, m)
	}
	sort.Strings(out.Unpriced)
	return out, nil
}

func add(t *hub.ClaudeCodeTotals, r hub.ClaudeCodeRow, total int64, cost float64) {
	t.Input += r.Input
	t.Output += r.Output
	t.CacheWrite += r.CacheWrite
	t.CacheRead += r.CacheRead
	t.Total += total
	t.Requests += r.Requests
	t.CostUSD += cost
}

// accountLabels names each account uuid: the user's own name for it, else
// the label of a claude-accounts slot signed in as it, else the email a
// computer reported. Unnamed accounts are left out.
func accountLabels(accounts []hub.ClaudeCodeAccount, names map[string]string) map[string]string {
	out := map[string]string{}
	for _, a := range accounts {
		if a.UUID != "" && a.Label != "" && out[a.UUID] == "" {
			out[a.UUID] = a.Label
		}
	}
	for _, a := range accounts {
		if a.UUID != "" && a.Slot != "" && a.Label != "" {
			out[a.UUID] = a.Label
		}
	}
	for id, n := range names {
		out[id] = n
	}
	return out
}

var accountID = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// NameClaudeAccount names an account for user; an empty name removes it.
func (s *Service) NameClaudeAccount(ctx context.Context, user, accountUUID, name string) error {
	name = strings.TrimSpace(name)
	if !accountID.MatchString(accountUUID) || utf8.RuneCountInString(name) > 64 || !utf8.ValidString(name) {
		return hub.ErrBadName
	}
	return s.o.Store.SetClaudeAccountName(ctx, user, accountUUID, name)
}

var modelName = regexp.MustCompile(`^claude-([a-z]+)-(\d+)(?:-(\d{1,2}))?(?:-\d{8})?(-fast)?$`)

// ModelLabel is a model id as people say it: claude-opus-5-5 is "Opus
// 5.5". Other ids stay as they are, and "" is "Unknown model".
func ModelLabel(model string) string {
	if model == "" {
		return "Unknown model"
	}
	m := modelName.FindStringSubmatch(model)
	if m == nil {
		return model
	}
	out := strings.ToUpper(m[1][:1]) + m[1][1:] + " " + m[2]
	if m[3] != "" {
		out += "." + m[3]
	}
	if m[4] != "" {
		out += " fast"
	}
	return out
}
