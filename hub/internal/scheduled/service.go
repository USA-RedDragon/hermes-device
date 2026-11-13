package scheduled

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/USA-RedDragon/talaria/hub/internal/notify"
	"golang.org/x/sync/errgroup"
)

// Access says whether a user may use a backend. *service.Service is one.
type Access interface {
	Allowed(ctx context.Context, user, backendID string) error
}

// Devices finds a paired device by id. *store.Store is one.
type Devices interface {
	Device(ctx context.Context, id string) (hub.Device, bool, error)
}

// Options are the dependencies of a Service.
type Options struct {
	Access     Access
	Backends   []config.Backend
	Requesters map[string]Requester
	// Devices names the browser devices of jobs. Nil: none are named.
	Devices Devices
	// Location is the time zone of the page. Default UTC.
	Location *time.Location
	// Timeout bounds each Hermes call. Default 20s.
	Timeout time.Duration
	// RunTimeout bounds a run that Run now starts. Default 6h.
	RunTimeout time.Duration
	Logger     *slog.Logger
	Now        func() time.Time
}

// Limits of what the hub sends to Hermes.
const (
	maxName     = 200
	maxSchedule = 200
	maxPrompt   = 20000
	// runsShown is how many runs each job shows.
	runsShown = 5
	// draftReason marks the paused draft job that checks a schedule while
	// the form is open. Hermes keeps it as the job's paused_reason.
	draftReason = "Talaria draft: not saved yet"
	// draftTTL is how long a draft the page left behind lives.
	draftTTL = time.Hour
	// cacheTTL is how long Hermes's time zone and default model are kept.
	cacheTTL = 5 * time.Minute
	// quickFail is how long Run now waits for Hermes to refuse a run.
	quickFail = 2 * time.Second
)

// Values of Hermes job records.
const (
	statePaused    = "paused"
	stateCompleted = "completed"
	statusError    = "error"
	statusNoDeliv  = "delivery_failed"
	deliverLocal   = "local"
)

type backend struct {
	cfg config.Backend
	c   client
	// shared is a backend more than one person may use: jobs show only to
	// the person who made them.
	shared bool
	// sandbox is a backend whose runs use the person's sandbox.
	sandbox bool
}

type cached struct {
	at    time.Time
	value string
}

// Service implements hub.Scheduled.
type Service struct {
	o        Options
	log      *slog.Logger
	backends map[string]*backend
	order    []string

	mu    sync.Mutex
	zones map[string]cached
	model map[string]cached
}

var _ hub.Scheduled = (*Service)(nil)

// New returns a Service.
func New(o Options) *Service {
	if o.Timeout == 0 {
		o.Timeout = 20 * time.Second
	}
	if o.RunTimeout == 0 {
		o.RunTimeout = 6 * time.Hour
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	s := &Service{o: o, log: o.Logger.With("component", "scheduled"), backends: map[string]*backend{},
		zones: map[string]cached{}, model: map[string]cached{}}
	for _, b := range o.Backends {
		r := o.Requesters[b.ID]
		if r == nil {
			continue
		}
		s.backends[b.ID] = &backend{cfg: b, c: client{r: r},
			shared:  len(b.Users) != 1 || len(b.Groups) > 0,
			sandbox: b.Devices && len(b.ServiceDevices) > 0}
		s.order = append(s.order, b.ID)
	}
	return s
}

func (s *Service) backend(ctx context.Context, user, backendID string) (*backend, error) {
	if err := s.o.Access.Allowed(ctx, user, backendID); err != nil {
		return nil, err
	}
	b := s.backends[backendID]
	if b == nil {
		return nil, hub.ErrNoScheduled
	}
	return b, nil
}

func (s *Service) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.o.Timeout)
}

// hermesErr turns a Hermes refusal into a hub error.
func hermesErr(err error) error {
	var he *HermesError
	if !errors.As(err, &he) {
		return err
	}
	switch he.Status {
	case http.StatusNotFound:
		return hub.ErrUnknownJob
	case http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusConflict:
		msg := strings.TrimSpace(he.Detail)
		if msg == "" {
			msg = "Hermes did not accept that"
		}
		return fmt.Errorf("%w: %s", hub.ErrBadValue, notify.Short(msg))
	}
	return err
}

// owner is who made a job: talaria's job_owners, else the hub user
// the hub recorded when it made the job. "" when unknown.
func owner(j rawJob, ch choices) string {
	if o, ok := ch.Owners[string(j.ID)]; ok && o.User != "" {
		return o.User
	}
	return string(j.Owner)
}

// visible reports whether user may see and change a job: one whose maker
// is that user, or on a backend only that user uses, one whose maker is
// not known.
func (b *backend) visible(user string, j rawJob, ch choices) bool {
	if isDraft(j) && string(j.Owner) != user {
		return false
	}
	if o := owner(j, ch); o != "" {
		return o == user
	}
	return !b.shared
}

func isDraft(j rawJob) bool { return string(j.PausedReason) == draftReason }

// readChoices reads talaria's browser.json under the jobs' Hermes
// home. A backend without one has no choices and no recorded owners.
func (s *Service) readChoices(ctx context.Context, b *backend, home string) choices {
	if !b.cfg.Devices {
		return choices{}
	}
	ctx, cancel := s.call(ctx)
	defer cancel()
	ch, err := b.c.choices(ctx, home)
	if err != nil {
		s.log.Debug("No browser device choices", "backend", b.cfg.ID, "error", err.Error())
		return choices{}
	}
	return ch
}

// Jobs lists the user's jobs on every backend they may use.
func (s *Service) Jobs(ctx context.Context, user string) (hub.Jobs, error) {
	now := s.o.Now()
	out := hub.Jobs{Zone: ZoneLabel(s.o.Location, now), Jobs: []hub.Job{}, Errors: map[string]string{}}
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	for _, id := range s.order {
		b := s.backends[id]
		if s.o.Access.Allowed(ctx, user, id) != nil {
			continue
		}
		g.Go(func() error {
			jobs, err := s.backendJobs(gctx, user, b)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				s.log.Warn("Failed to read scheduled jobs", "backend", id, "error", err.Error())
				out.Errors[b.cfg.Name] = "Could not read the scheduled jobs of " + b.cfg.Name
				return nil
			}
			out.Jobs = append(out.Jobs, jobs...)
			return nil
		})
	}
	_ = g.Wait()
	slices.SortStableFunc(out.Jobs, func(a, b hub.Job) int {
		if (a.NextAt == "") != (b.NextAt == "") {
			if a.NextAt == "" {
				return 1
			}
			return -1
		}
		return cmp.Or(strings.Compare(a.NextAt, b.NextAt), strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)))
	})
	return out, nil
}

func (s *Service) backendJobs(ctx context.Context, user string, b *backend) ([]hub.Job, error) {
	cctx, cancel := s.call(ctx)
	raw, err := b.c.list(cctx)
	cancel()
	if err != nil {
		return nil, err
	}
	home := ""
	for _, j := range raw {
		if home = string(j.HermesHome); home != "" {
			break
		}
	}
	ch := s.readChoices(ctx, b, home)
	var mine []rawJob
	for _, j := range raw {
		if isDraft(j) {
			s.dropStaleDraft(ctx, b, j)
			continue
		}
		if b.visible(user, j, ch) {
			mine = append(mine, j)
		}
	}
	if len(mine) == 0 {
		return nil, nil
	}
	defModel := s.defaultModel(ctx, b)
	jobs := make([]hub.Job, len(mine))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i, j := range mine {
		g.Go(func() error {
			jobs[i] = s.view(gctx, user, b, j, ch, defModel, s.readRuns(gctx, b, string(j.ID)))
			return nil
		})
	}
	_ = g.Wait()
	return jobs, nil
}

// dropStaleDraft removes a draft that a page left behind.
func (s *Service) dropStaleDraft(ctx context.Context, b *backend, j rawJob) {
	created, ok := ParseTime(string(j.CreatedAt), time.UTC)
	if !ok || s.o.Now().Sub(created) < draftTTL {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.o.Timeout)
		defer cancel()
		if err := b.c.remove(ctx, string(j.ID)); err != nil {
			s.log.Debug("Failed to remove a stale draft job", "backend", b.cfg.ID, "error", err.Error())
		}
	}()
}

func (s *Service) readRuns(ctx context.Context, b *backend, id string) []rawRun {
	ctx, cancel := s.call(ctx)
	defer cancel()
	runs, err := b.c.runs(ctx, id, runsShown)
	if err != nil {
		s.log.Debug("Failed to read job runs", "backend", b.cfg.ID, "error", err.Error())
		return nil
	}
	return runs
}

func (s *Service) cachedValue(cache map[string]cached, key string, read func() (string, error)) string {
	s.mu.Lock()
	c, ok := cache[key]
	s.mu.Unlock()
	if ok && s.o.Now().Sub(c.at) < cacheTTL {
		return c.value
	}
	v, err := read()
	if err != nil {
		return c.value
	}
	s.mu.Lock()
	cache[key] = cached{at: s.o.Now(), value: v}
	s.mu.Unlock()
	return v
}

func (s *Service) defaultModel(ctx context.Context, b *backend) string {
	return s.cachedValue(s.model, b.cfg.ID, func() (string, error) {
		ctx, cancel := s.call(ctx)
		defer cancel()
		return b.c.defaultModel(ctx)
	})
}

// hermesZone is the zone Hermes counts cron times in: its configured
// timezone, else the page's.
func (s *Service) hermesZone(ctx context.Context, b *backend) *time.Location {
	name := s.cachedValue(s.zones, b.cfg.ID, func() (string, error) {
		ctx, cancel := s.call(ctx)
		defer cancel()
		return b.c.timezone(ctx)
	})
	if name != "" {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	return s.o.Location
}

// runsIn says where a job's runs work: the sandbox or the Hermes server,
// and the Firefox of the computer talaria gave the job.
func (s *Service) runsIn(ctx context.Context, user string, b *backend, id string, ch choices) string {
	where := "Hermes server"
	if b.sandbox {
		where = "Sandbox"
	}
	choice, ok := ch.Jobs[id]
	if !ok || choice.DeviceID == "" {
		return where
	}
	if s.o.Devices == nil {
		return where + ", Firefox on a computer"
	}
	d, found, err := s.o.Devices.Device(ctx, choice.DeviceID)
	switch {
	case err != nil || !found:
		return where + ", Firefox on a removed computer"
	case d.User != user:
		return where + ", Firefox on another person's computer"
	}
	return where + ", Firefox on " + d.Name
}

func resultsText(results string) string {
	switch results {
	case hub.ResultsHub:
		return "This hub only"
	case hub.ResultsTopic:
		return "Telegram, its own topic"
	case hub.ResultsOrigin:
		return "The chat it was made in"
	}
	return "Telegram, General topic"
}

// results is a job's results choice: the one the hub set, else where
// Hermes delivers it.
func results(j rawJob) (string, string) {
	switch r := string(j.Results); r {
	case hub.ResultsHub, hub.ResultsGeneral, hub.ResultsTopic:
		return r, resultsText(r)
	}
	switch d := strings.TrimSpace(string(j.Deliver)); d {
	case "", deliverLocal:
		return hub.ResultsGeneral, resultsText(hub.ResultsGeneral)
	case "origin":
		return hub.ResultsOrigin, resultsText(hub.ResultsOrigin)
	default:
		return "", "Hermes sends them to " + d
	}
}

func (s *Service) view(ctx context.Context, user string, b *backend, j rawJob, ch choices, defModel string, runs []rawRun) hub.Job {
	now := s.o.Now()
	loc := s.o.Location
	out := hub.Job{BackendID: b.cfg.ID, Backend: b.cfg.Name, ID: string(j.ID), Name: string(j.Name), Prompt: string(j.Prompt),
		Schedule: cmp.Or(string(j.ScheduleDisplay), string(j.Schedule.Display), string(j.Schedule.Expr)),
		Once:     j.Schedule.Kind == kindOnce, Skills: []string(j.Skills), Locked: len(j.podFields()) > 0,
		Model: hub.JobModel{Provider: string(j.Provider), Model: string(j.Model)}, Runs: []hub.JobRun{}}
	if out.Skills == nil {
		out.Skills = []string{}
	}
	if j.Schedule.Kind == kindCron && string(j.Schedule.Expr) != out.Schedule {
		out.Expr = string(j.Schedule.Expr)
	}
	out.Results, out.ResultsText = results(j)
	out.RunsIn = s.runsIn(ctx, user, b, out.ID, ch)
	switch {
	case j.Model == "" && defModel != "":
		out.ModelLabel = "Follows the chat model (" + defModel + ")"
	case j.Model == "":
		out.ModelLabel = "Follows the chat model"
	default:
		out.ModelLabel = string(j.Model) + " (pinned)"
	}
	for i, r := range runs {
		out.Runs = append(out.Runs, runView(r, i == 0, j, loc))
	}
	if len(out.Runs) > 0 {
		out.Last = out.Runs[0].Result
	}
	paused := j.State == statePaused || !j.enabled()
	running := len(out.Runs) > 0 && out.Runs[0].Running
	out.State = hub.JobOK
	switch {
	case running:
		out.State, out.Flag = hub.JobRunning, "Running"
	case paused:
		out.State, out.Flag = hub.JobPaused, "Paused"
	case j.State == stateCompleted:
		out.State, out.Flag = hub.JobPaused, "Done"
	case j.LastStatus == statusError || j.State == statusError:
		if notify.NeedsLogin(string(j.LastError)) {
			out.State, out.Flag = hub.JobWarn, "Needs login"
		} else {
			out.State, out.Flag = hub.JobFailed, "Last run failed"
		}
	case j.LastStatus == statusNoDeliv:
		out.State, out.Flag = hub.JobFailed, "Result not delivered"
	}
	if next, ok := ParseTime(string(j.NextRunAt), loc); ok && !paused && j.State != stateCompleted {
		out.NextAt = next.UTC().Format(time.RFC3339)
		out.Next = When(next, loc)
		out.NextRel = Relative(now, next, loc)
	}
	return out
}

// runView is one run row. A run's chat holds its result; the row says
// how it ended, with the error of the latest run when it failed.
func runView(r rawRun, latest bool, j rawJob, loc *time.Location) hub.JobRun {
	start := epoch(float64(r.StartedAt))
	end := epoch(float64(cmp.Or(r.EndedAt, r.LastActive)))
	out := hub.JobRun{Running: bool(r.SchedulerOwned) || (bool(r.IsActive) && r.EndedAt == 0)}
	if !start.IsZero() {
		out.Started = When(start, loc)
		out.StartedAt = start.UTC().Format(time.RFC3339)
		if !out.Running && end.After(start) {
			out.Duration = Duration(end.Sub(start))
		}
	}
	title := strings.TrimSpace(string(r.Title))
	label, _, _ := strings.Cut(title, " · ")
	if r.Source == "cron_output" {
		out.Result = notify.Short(title)
		out.Failed = label == "FAILED" || label == "MISSED"
		return out
	}
	out.SessionID = string(r.ID)
	switch {
	case out.Running:
		out.Result = "Running"
	case label == "CANCELLED":
		out.Result = "Stopped: " + notify.Short(strings.TrimPrefix(title, label+" · "))
	case latest && j.LastStatus == statusError:
		out.Result, out.Failed = "Failed: "+notify.Short(string(j.LastError)), true
	case latest && j.LastStatus == statusNoDeliv:
		out.Result, out.Failed = "Ran, but the result was not delivered", true
	default:
		out.Result = "Finished"
	}
	return out
}

// find reads one of the user's jobs. A job the user cannot see is
// ErrUnknownJob, as one that does not exist.
func (s *Service) find(ctx context.Context, user, backendID, jobID string) (*backend, rawJob, choices, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return nil, rawJob{}, choices{}, err
	}
	if strings.TrimSpace(jobID) == "" {
		return nil, rawJob{}, choices{}, hub.ErrUnknownJob
	}
	cctx, cancel := s.call(ctx)
	j, err := b.c.get(cctx, jobID)
	cancel()
	if err != nil {
		return nil, rawJob{}, choices{}, hermesErr(err)
	}
	// Hermes also finds a job by name; only its id counts here.
	if string(j.ID) != jobID {
		return nil, rawJob{}, choices{}, hub.ErrUnknownJob
	}
	ch := s.readChoices(ctx, b, string(j.HermesHome))
	if !b.visible(user, j, ch) {
		return nil, rawJob{}, choices{}, hub.ErrUnknownJob
	}
	return b, j, ch, nil
}

func (s *Service) fullView(ctx context.Context, user string, b *backend, j rawJob, ch choices) hub.Job {
	return s.view(ctx, user, b, j, ch, s.defaultModel(ctx, b), s.readRuns(ctx, b, string(j.ID)))
}

// JobRuns lists a job's latest runs.
func (s *Service) JobRuns(ctx context.Context, user, backendID, jobID string) ([]hub.JobRun, error) {
	b, j, _, err := s.find(ctx, user, backendID, jobID)
	if err != nil || isDraft(j) {
		return nil, cmp.Or(err, hub.ErrUnknownJob)
	}
	out := []hub.JobRun{}
	for i, r := range s.readRuns(ctx, b, jobID) {
		out = append(out, runView(r, i == 0, j, s.o.Location))
	}
	return out, nil
}

// JobOptions lists the models a job may pin and where its runs work.
func (s *Service) JobOptions(ctx context.Context, user, backendID string) (hub.JobOptions, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return hub.JobOptions{}, err
	}
	out := hub.JobOptions{DefaultModel: s.defaultModel(ctx, b), Providers: []hub.ModelProvider{}, RunsIn: "Hermes server"}
	if b.sandbox {
		out.RunsIn = "Sandbox"
	}
	cctx, cancel := s.call(ctx)
	opts, err := b.c.models(cctx)
	cancel()
	if err != nil {
		return out, nil //nolint:nilerr // the form still works with the chat model
	}
	for _, p := range opts.Providers {
		if p.Authenticated != nil && !*p.Authenticated {
			continue
		}
		hp := hub.ModelProvider{ID: p.Slug, Name: cmp.Or(p.Name, p.Slug), Models: []hub.ModelChoice{}}
		if p.Slug == hub.ClaudeAPIProvider {
			hp.Name = hub.ClaudeAPIName
		}
		for _, m := range p.Models {
			if !slices.Contains(p.UnavailableModels, m) {
				hp.Models = append(hp.Models, hub.ModelChoice{ID: m})
			}
		}
		if len(hp.Models) > 0 {
			out.Providers = append(out.Providers, hp)
		}
	}
	return out, nil
}

// clean trims a field and checks its length; required fields must not
// be empty.
func clean(v *string, what string, limit int, required bool) (string, error) {
	if v == nil {
		return "", nil
	}
	t := strings.TrimSpace(*v)
	switch {
	case required && t == "":
		return "", fmt.Errorf("%w: %s is empty", hub.ErrBadValue, what)
	case len([]rune(t)) > limit:
		return "", fmt.Errorf("%w: %s is longer than %d characters", hub.ErrBadValue, what, limit)
	}
	return t, nil
}

// updates turns the form's fields into Hermes job updates. It sends only
// fields that run nowhere but in the agent: never a script, workdir,
// base_url or other field that runs or fetches on the Hermes server.
func updates(in hub.JobInput) (map[string]any, error) {
	out := map[string]any{}
	for _, f := range []struct {
		v        *string
		key, txt string
		limit    int
	}{
		{in.Name, "name", "the name", maxName},
		{in.Schedule, "schedule", "the schedule", maxSchedule},
		{in.Prompt, "prompt", "the instructions", maxPrompt},
	} {
		if f.v == nil {
			continue
		}
		t, err := clean(f.v, f.txt, f.limit, true)
		if err != nil {
			return nil, err
		}
		out[f.key] = t
	}
	if in.Model != nil {
		model, provider := strings.TrimSpace(in.Model.Model), strings.TrimSpace(in.Model.Provider)
		if model == "" {
			out["model"], out["provider"] = nil, nil
		} else {
			if provider == "" || len(model) > maxName || len(provider) > maxName {
				return nil, fmt.Errorf("%w: pick a model from the list", hub.ErrBadValue)
			}
			out["model"], out["provider"] = model, provider
		}
	}
	if in.Results != nil {
		switch r := *in.Results; r {
		case hub.ResultsHub, hub.ResultsGeneral, hub.ResultsTopic:
			out[hub.JobResultsKey] = r
			out["deliver"] = deliverLocal
		default:
			return nil, fmt.Errorf("%w: pick where the results go", hub.ErrBadValue)
		}
	}
	return out, nil
}

// isDraftOf reports whether id is the user's draft on a backend.
func (s *Service) isDraftOf(ctx context.Context, user string, b *backend, id string) bool {
	if id == "" {
		return false
	}
	cctx, cancel := s.call(ctx)
	j, err := b.c.get(cctx, id)
	cancel()
	return err == nil && string(j.ID) == id && isDraft(j) && string(j.Owner) == user
}

// PreviewSchedule has Hermes read a schedule. Hermes parses it on a paused
// draft job, which never runs; the next times follow from what Hermes
// made of it (nextRuns).
func (s *Service) PreviewSchedule(ctx context.Context, user, backendID, text, draft string) (hub.SchedulePreview, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return hub.SchedulePreview{}, err
	}
	sched, err := clean(&text, "the schedule", maxSchedule, true)
	if err != nil {
		return hub.SchedulePreview{OK: false, Error: "Type when the job should run.", Draft: draft}, nil //nolint:nilerr // the page shows it
	}
	var j rawJob
	if s.isDraftOf(ctx, user, b, draft) {
		cctx, cancel := s.call(ctx)
		j, err = b.c.update(cctx, draft, map[string]any{"schedule": sched})
		cancel()
	} else {
		draft = ""
		cctx, cancel := s.call(ctx)
		j, err = b.c.create(cctx, createBody{Name: "Talaria draft", Schedule: sched, Prompt: "Draft", Deliver: deliverLocal,
			Paused: true, PausedReason: draftReason})
		cancel()
		if err == nil {
			draft = string(j.ID)
			cctx, cancel := s.call(ctx)
			j, err = b.c.update(cctx, draft, map[string]any{hub.JobOwnerKey: user})
			cancel()
		}
	}
	var he *HermesError
	if errors.As(err, &he) && (he.Status == http.StatusBadRequest || he.Status == http.StatusUnprocessableEntity) {
		return hub.SchedulePreview{OK: false, Error: notify.Short(he.Detail), Draft: draft}, nil
	}
	if err != nil {
		return hub.SchedulePreview{}, hermesErr(err)
	}
	out := hub.SchedulePreview{OK: true, Display: cmp.Or(string(j.ScheduleDisplay), sched), Draft: draft}
	if j.Schedule.Kind == kindCron {
		out.Expr = string(j.Schedule.Expr)
	}
	if next, ok := nextRuns(j.Schedule, s.o.Now(), s.hermesZone(ctx, b), 2); ok {
		for _, t := range next {
			out.Next = append(out.Next, When(t, s.o.Location))
		}
	}
	return out, nil
}

// DiscardDraft removes the user's draft.
func (s *Service) DiscardDraft(ctx context.Context, user, backendID, draft string) error {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return err
	}
	if !s.isDraftOf(ctx, user, b, draft) {
		return nil
	}
	cctx, cancel := s.call(ctx)
	defer cancel()
	return hermesErr(b.c.remove(cctx, draft))
}

// CreateJob makes a job. Hermes makes it paused first, the hub records who
// made it and where its results go, then resumes it; a job that cannot be
// resumed is removed again.
func (s *Service) CreateJob(ctx context.Context, user, backendID string, in hub.JobInput) (hub.Job, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return hub.Job{}, err
	}
	if in.Name == nil || in.Schedule == nil || in.Prompt == nil {
		return hub.Job{}, fmt.Errorf("%w: a job needs a name, a schedule and instructions", hub.ErrBadValue)
	}
	if in.Results == nil {
		r := hub.ResultsGeneral
		in.Results = &r
	}
	up, err := updates(in)
	if err != nil {
		return hub.Job{}, err
	}
	up[hub.JobOwnerKey] = user
	var id string
	if s.isDraftOf(ctx, user, b, in.Draft) {
		id = in.Draft
	} else {
		cctx, cancel := s.call(ctx)
		j, err := b.c.create(cctx, createBody{Name: strings.TrimSpace(*in.Name), Schedule: strings.TrimSpace(*in.Schedule),
			Prompt: strings.TrimSpace(*in.Prompt), Deliver: deliverLocal, Paused: true, PausedReason: draftReason})
		cancel()
		if err != nil {
			return hub.Job{}, hermesErr(err)
		}
		id = string(j.ID)
	}
	up["paused_reason"] = nil
	cctx, cancel := s.call(ctx)
	j, err := b.c.update(cctx, id, up)
	if err == nil {
		j, err = b.c.action(cctx, id, "resume")
	}
	cancel()
	if err != nil {
		rctx, rcancel := s.call(context.WithoutCancel(ctx))
		if rerr := b.c.remove(rctx, id); rerr != nil {
			s.log.Warn("Failed to remove a job that could not be made", "backend", backendID, "error", rerr.Error())
		}
		rcancel()
		return hub.Job{}, hermesErr(err)
	}
	return s.fullView(ctx, user, b, j, s.readChoices(ctx, b, string(j.HermesHome))), nil
}

// UpdateJob changes a job's name, schedule, instructions, model or where
// its results go.
func (s *Service) UpdateJob(ctx context.Context, user, backendID, jobID string, in hub.JobInput) (hub.Job, error) {
	b, j, ch, err := s.find(ctx, user, backendID, jobID)
	if err != nil {
		return hub.Job{}, err
	}
	if isDraft(j) {
		return hub.Job{}, hub.ErrUnknownJob
	}
	if len(j.podFields()) > 0 {
		return hub.Job{}, hub.ErrJobLocked
	}
	up, err := updates(in)
	if err != nil {
		return hub.Job{}, err
	}
	if len(up) > 0 {
		cctx, cancel := s.call(ctx)
		j, err = b.c.update(cctx, jobID, up)
		cancel()
		if err != nil {
			return hub.Job{}, hermesErr(err)
		}
	}
	if in.Draft != "" {
		if err := s.DiscardDraft(ctx, user, backendID, in.Draft); err != nil {
			s.log.Debug("Failed to remove a draft job", "backend", backendID, "error", err.Error())
		}
	}
	return s.fullView(ctx, user, b, j, ch), nil
}

func (s *Service) act(ctx context.Context, user, backendID, jobID, verb string, locked bool) (hub.Job, error) {
	b, j, ch, err := s.find(ctx, user, backendID, jobID)
	if err != nil {
		return hub.Job{}, err
	}
	if isDraft(j) {
		return hub.Job{}, hub.ErrUnknownJob
	}
	if locked && len(j.podFields()) > 0 {
		return hub.Job{}, hub.ErrJobLocked
	}
	cctx, cancel := s.call(ctx)
	j, err = b.c.action(cctx, jobID, verb)
	cancel()
	if err != nil {
		return hub.Job{}, hermesErr(err)
	}
	return s.fullView(ctx, user, b, j, ch), nil
}

// PauseJob pauses a job; a run in progress goes on.
func (s *Service) PauseJob(ctx context.Context, user, backendID, jobID string) (hub.Job, error) {
	return s.act(ctx, user, backendID, jobID, "pause", false)
}

// ResumeJob resumes a paused job.
func (s *Service) ResumeJob(ctx context.Context, user, backendID, jobID string) (hub.Job, error) {
	return s.act(ctx, user, backendID, jobID, "resume", true)
}

// RunJob starts a run now. Hermes answers its trigger only when the run
// ends, so the run goes on in the background; a refusal that comes at
// once is the answer. Hermes resumes a paused job that it runs, so the
// hub pauses it again as soon as the run is claimed.
func (s *Service) RunJob(ctx context.Context, user, backendID, jobID string) error {
	b, j, _, err := s.find(ctx, user, backendID, jobID)
	if err != nil {
		return err
	}
	if isDraft(j) {
		return hub.ErrUnknownJob
	}
	if len(j.podFields()) > 0 {
		return hub.ErrJobLocked
	}
	paused := j.State == statePaused || !j.enabled()
	done := make(chan error, 1)
	go func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.o.RunTimeout)
		defer cancel()
		err := b.c.trigger(rctx, jobID)
		if err != nil {
			s.log.Info("Run now ended with an error", "backend", backendID, "error", err.Error())
		}
		done <- err
	}()
	if paused {
		go s.keepPaused(context.WithoutCancel(ctx), b, jobID, done)
		return nil
	}
	select {
	case err := <-done:
		return hermesErr(err)
	case <-time.After(quickFail):
		return nil
	case <-ctx.Done():
		return nil
	}
}

// keepPaused pauses again a paused job that a run resumed. It watches
// the job until Hermes claimed the run (the job is enabled) or the run
// ended.
func (s *Service) keepPaused(ctx context.Context, b *backend, jobID string, done <-chan error) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(time.Minute)
	finished := false
	for {
		cctx, cancel := s.call(ctx)
		j, err := b.c.get(cctx, jobID)
		cancel()
		if err == nil && j.enabled() && j.State != statePaused {
			cctx, cancel := s.call(ctx)
			if _, err := b.c.action(cctx, jobID, "pause"); err != nil {
				s.log.Warn("Failed to pause a job again after Run now", "backend", b.cfg.ID, "error", err.Error())
			}
			cancel()
			return
		}
		if finished {
			return
		}
		select {
		case <-done:
			finished = true
		case <-deadline:
			return
		case <-tick.C:
		}
	}
}

// CancelJob stops a job's run in progress. false: nothing ran.
func (s *Service) CancelJob(ctx context.Context, user, backendID, jobID string) (bool, error) {
	b, j, _, err := s.find(ctx, user, backendID, jobID)
	if err != nil {
		return false, err
	}
	if isDraft(j) {
		return false, hub.ErrUnknownJob
	}
	cctx, cancel := s.call(ctx)
	defer cancel()
	ok, err := b.c.cancel(cctx, jobID)
	return ok, hermesErr(err)
}

// DeleteJob removes a job. Its runs' chats stay.
func (s *Service) DeleteJob(ctx context.Context, user, backendID, jobID string) error {
	b, _, _, err := s.find(ctx, user, backendID, jobID)
	if err != nil {
		return err
	}
	cctx, cancel := s.call(ctx)
	defer cancel()
	return hermesErr(b.c.remove(cctx, jobID))
}
