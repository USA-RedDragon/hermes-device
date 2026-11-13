// Package scheduledtest has a fake of the Hermes dashboard's cron routes
// (hermes_cli/web_routers/cron.py) for tests, mounted on a
// gatewaytest.Gateway as the Webapp listener serves them.
package scheduledtest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway/gatewaytest"
)

// Home is the fake's HERMES_HOME.
const Home = "/opt/data/hermes"

// NextRun is the next_run_at the fake gives every active job.
const NextRun = "2026-11-12T09:00:00-06:00"

// DefaultModel is the fake's chat model.
const DefaultModel = "qwen-big"

const (
	kModel     = "model"
	kName      = "name"
	kProvider  = "provider"
	kDisplay   = "display"
	kExpr      = "expr"
	kKind      = "kind"
	kSchedule  = "schedule"
	kindCron   = "cron"
	daily      = "0 9 * * *"
	scheduled  = "scheduled"
	statePause = "paused"
)

// Dash is the fake dashboard. Its fields may be changed under Lock.
type Dash struct {
	sync.Mutex
	// Jobs are the job records, in order.
	Jobs []map[string]any
	// Runs are each job's run rows, newest first.
	Runs map[string][]map[string]any
	// Calls are the requests served, as "METHOD path".
	Calls []string
	// Bodies are the JSON bodies of the writes, as Calls lists them.
	Bodies []map[string]any
	// Hold makes a trigger wait until it is closed.
	Hold chan struct{}
	// Triggered counts the runs started.
	Triggered int
	// Activated is closed when a trigger resumed a paused job.
	Activated chan struct{}
	n         int
	// Now is the fake's clock.
	Now func() time.Time
}

// New mounts a fake dashboard with no jobs on gw.
func New(gw *gatewaytest.Gateway) *Dash {
	d := &Dash{Runs: map[string][]map[string]any{}, Activated: make(chan struct{}), Now: time.Now}
	const j = "/api/cron/jobs"
	for pattern, h := range map[string]http.HandlerFunc{
		"GET " + j:                   d.list,
		"POST " + j:                  d.create,
		"GET " + j + "/{id}":         d.get,
		"PUT " + j + "/{id}":         d.update,
		"DELETE " + j + "/{id}":      d.remove,
		"GET " + j + "/{id}/runs":    d.runs,
		"POST " + j + "/{id}/{verb}": d.action,
		"GET /api/model/info":        d.modelInfo,
		"GET /api/model/options":     d.modelOptions,
		"GET /api/config":            d.config,
	} {
		gw.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var raw []byte
			if r.Body != nil {
				raw, _ = io.ReadAll(r.Body)
			}
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			d.Lock()
			d.Calls = append(d.Calls, r.Method+" "+r.URL.Path)
			d.Bodies = append(d.Bodies, body)
			d.Unlock()
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			h(w, r)
		}), true)
	}
	return d
}

func readBody(r *http.Request) map[string]any {
	var b map[string]any
	_ = json.NewDecoder(r.Body).Decode(&b)
	if b == nil {
		b = map[string]any{}
	}
	return b
}

func answer(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func refuse(w http.ResponseWriter, code int, msg string) {
	answer(w, code, map[string]string{"detail": msg})
}

// Add adds a job record. Missing fields get the defaults of an active
// prompt job.
func (d *Dash) Add(job map[string]any) map[string]any {
	d.Lock()
	defer d.Unlock()
	rec := map[string]any{kName: "job", "prompt": "do it", "skills": []string{}, kModel: nil, kProvider: nil,
		kSchedule:          map[string]any{kKind: kindCron, kExpr: daily, kDisplay: daily},
		"schedule_display": daily, "enabled": true, "state": scheduled, "next_run_at": NextRun,
		"last_run_at": nil, "last_status": nil, "last_error": nil, "deliver": "local", "paused_reason": nil,
		"created_at": d.Now().Format(time.RFC3339)}
	for k, v := range job {
		rec[k] = v
	}
	if rec["id"] == nil {
		d.n++
		rec["id"] = fmt.Sprintf("job%03d", d.n)
	}
	d.Jobs = append(d.Jobs, rec)
	return rec
}

// Job returns a copy of a job record, or nil.
func (d *Dash) Job(id string) map[string]any {
	d.Lock()
	defer d.Unlock()
	if j := d.find(id, false); j != nil {
		out := map[string]any{}
		for k, v := range j {
			out[k] = v
		}
		return out
	}
	return nil
}

// Count counts the calls that start with prefix, such as "POST /api/cron/jobs".
func (d *Dash) Count(prefix string) int {
	d.Lock()
	defer d.Unlock()
	n := 0
	for _, c := range d.Calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// find looks a job up by id or, as Hermes does, by name. Call with the lock.
func (d *Dash) find(ref string, byName bool) map[string]any {
	for _, j := range d.Jobs {
		if j["id"] == ref || (byName && j[kName] == ref) {
			return j
		}
	}
	return nil
}

func annotate(j map[string]any) map[string]any {
	out := map[string]any{"hermes_home": Home, "profile": "default"}
	for k, v := range j {
		out[k] = v
	}
	return out
}

func (d *Dash) list(w http.ResponseWriter, _ *http.Request) {
	d.Lock()
	out := make([]map[string]any, 0, len(d.Jobs))
	for _, j := range d.Jobs {
		out = append(out, annotate(j))
	}
	d.Unlock()
	answer(w, http.StatusOK, out)
}

var cronExpr = regexp.MustCompile(`^[A-Za-z\d*\-,/]+$`)

// parse is a small part of cron/jobs.py parse_schedule.
func parse(s string, now time.Time) (map[string]any, error) {
	s = strings.TrimSpace(s)
	switch low := strings.ToLower(s); {
	case low == "every monday 9am":
		return map[string]any{kKind: kindCron, kExpr: "0 9 * * 1", kDisplay: s}, nil
	case strings.HasPrefix(low, "every ") && strings.HasSuffix(low, "h"):
		var n int
		if _, err := fmt.Sscanf(low, "every %dh", &n); err == nil && n > 0 {
			return map[string]any{kKind: "interval", "minutes": n * 60, kDisplay: fmt.Sprintf("every %dm", n*60)}, nil
		}
	case strings.HasPrefix(low, "in ") && strings.HasSuffix(low, "m"):
		var n int
		if _, err := fmt.Sscanf(low, "in %dm", &n); err == nil && n > 0 {
			at := now.Add(time.Duration(n) * time.Minute)
			return map[string]any{kKind: "once", "run_at": at.Format(time.RFC3339), kDisplay: "once in " + s[3:]}, nil
		}
	}
	if f := strings.Fields(s); len(f) == 5 {
		ok := true
		for _, p := range f {
			ok = ok && cronExpr.MatchString(p)
		}
		if ok {
			return map[string]any{kKind: kindCron, kExpr: s, kDisplay: s}, nil
		}
	}
	return nil, errors.New("Invalid schedule '" + s + "'. Use:\n  - Interval: '30m', 'every 30m', 'every 2h' (recurring)") //nolint:staticcheck // Hermes's own words
}

func (d *Dash) create(w http.ResponseWriter, r *http.Request) {
	b := readBody(r)
	sched, err := parse(fmt.Sprint(b[kSchedule]), d.Now())
	if err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return
	}
	paused, _ := b["paused"].(bool)
	rec := map[string]any{kName: b["name"], "prompt": b["prompt"], kSchedule: sched, "schedule_display": sched[kDisplay],
		"deliver": b["deliver"], kModel: b["model"], kProvider: b[kProvider], "enabled": !paused,
		"state": scheduled, "next_run_at": NextRun, "paused_reason": nil}
	if paused {
		rec["state"], rec["next_run_at"], rec["paused_reason"] = statePause, nil, b["paused_reason"]
	}
	for _, k := range []string{"script", "workdir", "no_agent", "base_url"} {
		if v, ok := b[k]; ok {
			rec[k] = v
		}
	}
	answer(w, http.StatusOK, annotate(d.Add(rec)))
}

func (d *Dash) get(w http.ResponseWriter, r *http.Request) {
	d.Lock()
	j := d.find(r.PathValue("id"), true)
	var out map[string]any
	if j != nil {
		out = annotate(j)
	}
	d.Unlock()
	if out == nil {
		refuse(w, http.StatusNotFound, "Job not found")
		return
	}
	answer(w, http.StatusOK, out)
}

func (d *Dash) update(w http.ResponseWriter, r *http.Request) {
	up, _ := readBody(r)["updates"].(map[string]any)
	d.Lock()
	defer d.Unlock()
	j := d.find(r.PathValue("id"), true)
	if j == nil {
		refuse(w, http.StatusNotFound, "Job not found")
		return
	}
	if s, ok := up[kSchedule]; ok {
		sched, err := parse(fmt.Sprint(s), d.Now())
		if err != nil {
			refuse(w, http.StatusBadRequest, err.Error())
			return
		}
		up[kSchedule], up["schedule_display"] = sched, sched[kDisplay]
	}
	for k, v := range up {
		j[k] = v
	}
	answer(w, http.StatusOK, annotate(j))
}

func (d *Dash) remove(w http.ResponseWriter, r *http.Request) {
	d.Lock()
	defer d.Unlock()
	for i, j := range d.Jobs {
		if j["id"] == r.PathValue("id") || j[kName] == r.PathValue("id") {
			d.Jobs = append(d.Jobs[:i], d.Jobs[i+1:]...)
			answer(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
	}
	refuse(w, http.StatusNotFound, "Job not found")
}

func (d *Dash) runs(w http.ResponseWriter, r *http.Request) {
	d.Lock()
	runs := d.Runs[r.PathValue("id")]
	d.Unlock()
	if runs == nil {
		runs = []map[string]any{}
	}
	answer(w, http.StatusOK, map[string]any{"runs": runs, "limit": 5})
}

func (d *Dash) action(w http.ResponseWriter, r *http.Request) {
	d.Lock()
	j := d.find(r.PathValue("id"), true)
	if j == nil {
		d.Unlock()
		refuse(w, http.StatusNotFound, "Job not found")
		return
	}
	switch r.PathValue("verb") {
	case "pause":
		j["enabled"], j["state"], j["paused_at"] = false, statePause, d.Now().Format(time.RFC3339)
	case "resume":
		if sched, _ := j[kSchedule].(map[string]any); sched[kKind] == "once" {
			if at, err := time.Parse(time.RFC3339, fmt.Sprint(sched["run_at"])); err == nil && at.Before(d.Now()) {
				d.Unlock()
				refuse(w, http.StatusBadRequest, "Cannot resume: one-shot time is in the past")
				return
			}
		}
		j["enabled"], j["state"], j["paused_reason"], j["paused_at"], j["next_run_at"] = true, scheduled, nil, nil, NextRun
	case "trigger":
		if j["enabled"] == false {
			j["enabled"], j["state"] = true, scheduled
			select {
			case <-d.Activated:
			default:
				close(d.Activated)
			}
		}
		d.Triggered++
		hold := d.Hold
		d.Unlock()
		if hold != nil {
			<-hold
		}
		d.Lock()
		j["last_run_at"] = d.Now().Format(time.RFC3339)
		out := annotate(j)
		d.Unlock()
		answer(w, http.StatusOK, out)
		return
	case "cancel":
		running := d.Hold != nil
		d.Unlock()
		answer(w, http.StatusOK, map[string]any{"cancelled": running, "job_id": j["id"]})
		return
	default:
		d.Unlock()
		refuse(w, http.StatusNotFound, "Not Found")
		return
	}
	out := annotate(j)
	d.Unlock()
	answer(w, http.StatusOK, out)
}

func (d *Dash) modelInfo(w http.ResponseWriter, _ *http.Request) {
	answer(w, http.StatusOK, map[string]any{kModel: DefaultModel, kProvider: "custom"})
}

func (d *Dash) modelOptions(w http.ResponseWriter, _ *http.Request) {
	yes, no := true, false
	answer(w, http.StatusOK, map[string]any{"providers": []map[string]any{
		{"slug": "custom", kName: "Local", "authenticated": &yes, "models": []string{DefaultModel, "qwen-small", "gone"},
			"unavailable_models": []string{"gone"}},
		{"slug": "cloud", kName: "Cloud", "authenticated": &no, "models": []string{"remote"}},
	}})
}

func (d *Dash) config(w http.ResponseWriter, _ *http.Request) {
	answer(w, http.StatusOK, map[string]any{"timezone": "America/Chicago", kModel: map[string]any{"default": DefaultModel}})
}
