// Package scheduled serves the Scheduled page: the Hermes cron jobs of each
// backend, read and changed through the Hermes dashboard's /api/cron routes
// with the hub's own dashboard login.
package scheduled

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Requester reaches one backend's dashboard with the hub's session.
// *gateway.Dialer is one.
type Requester interface {
	Request(ctx context.Context, method, path string, body []byte) (*http.Response, error)
	// RequestSlow has no time limit of its own: Hermes answers a run of a
	// job only when the run ends.
	RequestSlow(ctx context.Context, method, path string, body []byte) (*http.Response, error)
	// File reads a file of the backend's server.
	File(ctx context.Context, path string) (*http.Response, error)
}

const (
	maxBody = 8 << 20
	jobsAPI = "/api/cron/jobs"
)

// HermesError is an answer of Hermes other than 2xx, with its detail.
type HermesError struct {
	Status int
	Detail string
}

func (e *HermesError) Error() string {
	return fmt.Sprintf("The agent answered %d: %s", e.Status, e.Detail)
}

// text is a JSON string that may be null or of another type, which reads
// as "", so one odd field never hides the whole job list.
type text string

func (t *text) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*t = text(s)
	} else {
		*t = ""
	}
	return nil
}

// flag is a JSON boolean that may be null or of another type.
type flag bool

func (f *flag) UnmarshalJSON(b []byte) error {
	var v bool
	*f = flag(json.Unmarshal(b, &v) == nil && v)
	return nil
}

// texts is a list of strings that may be null, one string or of another
// type.
type texts []string

func (t *texts) UnmarshalJSON(b []byte) error {
	var list []string
	if json.Unmarshal(b, &list) == nil {
		*t = list
		return nil
	}
	var one string
	if json.Unmarshal(b, &one) == nil && one != "" {
		*t = []string{one}
		return nil
	}
	*t = nil
	return nil
}

// number is a JSON number that may be null or of another type.
type number float64

func (n *number) UnmarshalJSON(b []byte) error {
	var v float64
	if json.Unmarshal(b, &v) == nil {
		*n = number(v)
	} else {
		*n = 0
	}
	return nil
}

// schedule is a job's parsed schedule (cron/jobs.py parse_schedule).
type schedule struct {
	Kind    text   `json:"kind"`
	Expr    text   `json:"expr"`
	Display text   `json:"display"`
	RunAt   text   `json:"run_at"`
	Minutes number `json:"minutes"`
}

// rawJob is the part of a Hermes cron job record (cron/jobs.py create_job,
// annotated by web_server_cron._annotate_cron_job) that the hub reads.
type rawJob struct {
	ID              text     `json:"id"`
	Name            text     `json:"name"`
	Prompt          text     `json:"prompt"`
	Skills          texts    `json:"skills"`
	Model           text     `json:"model"`
	Provider        text     `json:"provider"`
	Schedule        schedule `json:"schedule"`
	ScheduleDisplay text     `json:"schedule_display"`
	Enabled         *bool    `json:"enabled"`
	State           text     `json:"state"`
	NextRunAt       text     `json:"next_run_at"`
	LastRunAt       text     `json:"last_run_at"`
	LastStatus      text     `json:"last_status"`
	LastError       text     `json:"last_error"`
	DeliveryError   text     `json:"last_delivery_error"`
	Deliver         text     `json:"deliver"`
	PausedReason    text     `json:"paused_reason"`
	Script          text     `json:"script"`
	MonitorScript   text     `json:"monitor_script"`
	MonitorURL      text     `json:"monitor_url"`
	Workdir         text     `json:"workdir"`
	Interpreter     text     `json:"interpreter"`
	NoAgent         flag     `json:"no_agent"`
	HermesHome      text     `json:"hermes_home"`
	Timezone        text     `json:"timezone"`
	Results         text     `json:"talaria_results"`
	Owner           text     `json:"talaria_owner"`
	CreatedAt       text     `json:"created_at"`
}

func (j rawJob) enabled() bool { return j.Enabled == nil || *j.Enabled }

// podFields are the stored fields of j that run or fetch on the Hermes
// server itself; talaria's CRON_JOB_POD_FIELDS, plus interpreter.
func (j rawJob) podFields() []string {
	var out []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"script", j.Script != ""}, {"monitor_script", j.MonitorScript != ""}, {"monitor_url", j.MonitorURL != ""},
		{"no_agent", bool(j.NoAgent)}, {"workdir", j.Workdir != ""}, {"interpreter", j.Interpreter != ""},
	} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// rawRun is one row of /api/cron/jobs/{id}/runs: an agent run's session
// in /api/sessions shape, or a run record without a session
// ("cron_output:" ids).
type rawRun struct {
	ID             text   `json:"id"`
	Source         text   `json:"source"`
	Title          text   `json:"title"`
	StartedAt      number `json:"started_at"`
	EndedAt        number `json:"ended_at"`
	LastActive     number `json:"last_active"`
	EndReason      text   `json:"end_reason"`
	IsActive       flag   `json:"is_active"`
	SchedulerOwned flag   `json:"scheduler_owned"`
}

// client calls one backend's cron routes.
type client struct {
	r Requester
}

func (c client) do(ctx context.Context, method, path string, in, out any) error {
	return c.send(ctx, false, method, path, in, out)
}

func (c client) send(ctx context.Context, slow bool, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	call := c.r.Request
	if slow {
		call = c.r.RequestSlow
	}
	resp, err := call(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HermesError{Status: resp.StatusCode, Detail: detail(raw)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// detail is the reason of a FastAPI error answer: a string detail, a
// detail object's message, or the first validation error.
func detail(raw []byte) string {
	var body struct {
		Detail json.RawMessage `json:"detail"`
		Error  string          `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return strings.TrimSpace(string(raw[:min(len(raw), 200)]))
	}
	var s string
	if json.Unmarshal(body.Detail, &s) == nil {
		return s
	}
	var obj struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(body.Detail, &obj) == nil && (obj.Message != "" || obj.Detail != "") {
		return obj.Message + obj.Detail
	}
	var list []struct {
		Msg string `json:"msg"`
	}
	if json.Unmarshal(body.Detail, &list) == nil && len(list) > 0 {
		return list[0].Msg
	}
	return body.Error
}

func jobPath(id string, tail string) string {
	return jobsAPI + "/" + url.PathEscape(id) + tail
}

func (c client) list(ctx context.Context) ([]rawJob, error) {
	var jobs []rawJob
	err := c.do(ctx, http.MethodGet, jobsAPI, nil, &jobs)
	return jobs, err
}

func (c client) get(ctx context.Context, id string) (rawJob, error) {
	var j rawJob
	err := c.do(ctx, http.MethodGet, jobPath(id, ""), nil, &j)
	return j, err
}

func (c client) runs(ctx context.Context, id string, limit int) ([]rawRun, error) {
	var out struct {
		Runs []rawRun `json:"runs"`
	}
	err := c.do(ctx, http.MethodGet, jobPath(id, fmt.Sprintf("/runs?limit=%d", limit)), nil, &out)
	return out.Runs, err
}

// createBody is the part of Hermes's CronJobCreate that the hub sends.
// It never sends a script, workdir, base_url or other field that runs or
// fetches on the Hermes server.
type createBody struct {
	Name         string `json:"name"`
	Schedule     string `json:"schedule"`
	Prompt       string `json:"prompt"`
	Deliver      string `json:"deliver"`
	Model        string `json:"model,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Paused       bool   `json:"paused"`
	PausedReason string `json:"paused_reason,omitempty"`
}

func (c client) create(ctx context.Context, b createBody) (rawJob, error) {
	var j rawJob
	err := c.do(ctx, http.MethodPost, jobsAPI, b, &j)
	return j, err
}

func (c client) update(ctx context.Context, id string, updates map[string]any) (rawJob, error) {
	var j rawJob
	err := c.do(ctx, http.MethodPut, jobPath(id, ""), map[string]any{"updates": updates}, &j)
	return j, err
}

func (c client) action(ctx context.Context, id, verb string) (rawJob, error) {
	var j rawJob
	err := c.do(ctx, http.MethodPost, jobPath(id, "/"+verb), nil, &j)
	return j, err
}

// trigger runs the job now. Hermes answers when the run ends.
func (c client) trigger(ctx context.Context, id string) error {
	return c.send(ctx, true, http.MethodPost, jobPath(id, "/trigger"), nil, nil)
}

func (c client) cancel(ctx context.Context, id string) (bool, error) {
	var out struct {
		Cancelled bool `json:"cancelled"`
	}
	err := c.do(ctx, http.MethodPost, jobPath(id, "/cancel"), nil, &out)
	return out.Cancelled, err
}

func (c client) remove(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, jobPath(id, ""), nil, nil)
}

// modelOptions reads /api/model/options, the REST twin of the gateway's
// model.options.
type modelOptions struct {
	Providers []struct {
		Slug              string   `json:"slug"`
		Name              string   `json:"name"`
		Authenticated     *bool    `json:"authenticated"`
		Models            []string `json:"models"`
		UnavailableModels []string `json:"unavailable_models"`
	} `json:"providers"`
}

func (c client) models(ctx context.Context) (modelOptions, error) {
	var out modelOptions
	err := c.do(ctx, http.MethodGet, "/api/model/options", nil, &out)
	return out, err
}

func (c client) defaultModel(ctx context.Context) (string, error) {
	var out struct {
		Model string `json:"model"`
	}
	err := c.do(ctx, http.MethodGet, "/api/model/info", nil, &out)
	return out.Model, err
}

// timezone is Hermes's configured time zone (config.yaml timezone), ""
// when unset.
func (c client) timezone(ctx context.Context) (string, error) {
	var out struct {
		Timezone text `json:"timezone"`
	}
	err := c.do(ctx, http.MethodGet, "/api/config", nil, &out)
	return string(out.Timezone), err
}

// choices are talaria's browser device choices and job owners
// ($HERMES_HOME/device/browser.json, talaria docs/browser.md
// section 6).
type choices struct {
	Version int `json:"version"`
	Jobs    map[string]struct {
		DeviceID string `json:"device_id"`
		User     string `json:"user"`
	} `json:"jobs"`
	Owners map[string]struct {
		User string `json:"user"`
	} `json:"job_owners"`
}

// choicesVersion is the browser.json version the hub reads.
const choicesVersion = 1

// errNoChoices is a backend without a readable browser.json.
var errNoChoices = errors.New("no browser device choices")

func (c client) choices(ctx context.Context, home string) (choices, error) {
	var out choices
	if home == "" {
		return out, errNoChoices
	}
	resp, err := c.r.File(ctx, strings.TrimRight(home, "/")+"/device/browser.json")
	if err != nil {
		return out, fmt.Errorf("%w: %w", errNoChoices, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return out, err
	}
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&out); err != nil || out.Version != choicesVersion {
		return choices{}, fmt.Errorf("%w: unreadable", errNoChoices)
	}
	return out, nil
}
