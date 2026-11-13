package hub

import (
	"context"
	"errors"
)

// Errors of the Scheduled page.
var (
	// ErrNoScheduled is a backend whose Hermes has no scheduled jobs API.
	ErrNoScheduled = errors.New("this backend has no scheduled jobs")
	// ErrUnknownJob is a job the user cannot see: it does not exist or
	// belongs to another user of a shared backend.
	ErrUnknownJob = errors.New("that scheduled job no longer exists")
	// ErrJobLocked is a change refused because the job has fields that
	// run on the Hermes server itself, such as a script.
	ErrJobLocked = errors.New("this job runs a script or other code on the server, so it can only be paused, stopped or deleted here")
)

// Results-go-to choices of a scheduled job, kept on the Hermes job as
// JobResultsKey.
const (
	ResultsHub     = "hub"
	ResultsGeneral = "general"
	ResultsTopic   = "topic"
	// ResultsOrigin is a job that a chat made, whose results go to that
	// chat. The hub does not set it.
	ResultsOrigin = "origin"
)

// Keys the hub keeps on a Hermes job record. Hermes keeps fields it does
// not know, and its agent tools cannot set them.
const (
	JobResultsKey = "talaria_results"
	JobOwnerKey   = "talaria_owner"
)

// Job states on the Scheduled page.
const (
	JobOK      = "ok"
	JobFailed  = "bad"
	JobWarn    = "warn"
	JobPaused  = "paused"
	JobRunning = "run"
)

// JobRun is one run of a scheduled job.
type JobRun struct {
	// SessionID is the run's chat, "" for a run without one (a script
	// run, or one whose record Hermes pruned).
	SessionID string `json:"session_id,omitempty"`
	Started   string `json:"started"`
	StartedAt string `json:"started_at"`
	Duration  string `json:"duration"`
	Result    string `json:"result"`
	Failed    bool   `json:"failed"`
	Running   bool   `json:"running"`
}

// JobModel is the model a job runs with. Model "" follows the chat model.
type JobModel struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Job is one scheduled job as the Scheduled page shows it. Times are in
// the hub's time zone.
type Job struct {
	BackendID   string   `json:"backend_id"`
	Backend     string   `json:"backend"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	State       string   `json:"state"`
	Flag        string   `json:"flag,omitempty"`
	Schedule    string   `json:"schedule"`
	Expr        string   `json:"expr,omitempty"`
	Once        bool     `json:"once"`
	NextAt      string   `json:"next_at,omitempty"`
	Next        string   `json:"next,omitempty"`
	NextRel     string   `json:"next_rel,omitempty"`
	RunsIn      string   `json:"runs_in"`
	Model       JobModel `json:"model"`
	ModelLabel  string   `json:"model_label"`
	Skills      []string `json:"skills"`
	Results     string   `json:"results"`
	ResultsText string   `json:"results_text"`
	Prompt      string   `json:"prompt"`
	// Locked is a job with fields that run on the Hermes server: only
	// pause, stop and delete are allowed.
	Locked bool     `json:"locked"`
	Last   string   `json:"last,omitempty"`
	Runs   []JobRun `json:"runs"`
}

// Jobs is the Scheduled page: the user's jobs on every backend, with the
// backends that could not be read in Errors.
type Jobs struct {
	Zone   string            `json:"zone"`
	Jobs   []Job             `json:"jobs"`
	Errors map[string]string `json:"errors"`
}

// JobInput is a new job or a change to one. A nil field is not changed.
type JobInput struct {
	Name     *string   `json:"name"`
	Schedule *string   `json:"schedule"`
	Prompt   *string   `json:"prompt"`
	Model    *JobModel `json:"model"`
	Results  *string   `json:"results"`
	// Draft is the preview draft of the form, which a new job takes over.
	Draft string `json:"draft"`
}

// SchedulePreview is how Hermes reads a schedule. Draft is the draft job
// that checked it, for the next preview and the save.
type SchedulePreview struct {
	OK      bool     `json:"ok"`
	Error   string   `json:"error,omitempty"`
	Display string   `json:"display,omitempty"`
	Expr    string   `json:"expr,omitempty"`
	Next    []string `json:"next,omitempty"`
	Draft   string   `json:"draft,omitempty"`
}

// JobOptions are the choices of the job form on one backend.
type JobOptions struct {
	DefaultModel string          `json:"default_model"`
	Providers    []ModelProvider `json:"providers"`
	RunsIn       string          `json:"runs_in"`
}

// Scheduled serves the Scheduled page: Hermes cron jobs, through each
// backend's dashboard API.
type Scheduled interface {
	Jobs(ctx context.Context, user string) (Jobs, error)
	JobRuns(ctx context.Context, user, backendID, jobID string) ([]JobRun, error)
	JobOptions(ctx context.Context, user, backendID string) (JobOptions, error)
	PreviewSchedule(ctx context.Context, user, backendID, schedule, draft string) (SchedulePreview, error)
	DiscardDraft(ctx context.Context, user, backendID, draft string) error
	CreateJob(ctx context.Context, user, backendID string, in JobInput) (Job, error)
	UpdateJob(ctx context.Context, user, backendID, jobID string, in JobInput) (Job, error)
	PauseJob(ctx context.Context, user, backendID, jobID string) (Job, error)
	ResumeJob(ctx context.Context, user, backendID, jobID string) (Job, error)
	RunJob(ctx context.Context, user, backendID, jobID string) error
	CancelJob(ctx context.Context, user, backendID, jobID string) (bool, error)
	DeleteJob(ctx context.Context, user, backendID, jobID string) error
}
