package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// cronJob is the part of a row of the Hermes dashboard's /api/cron/jobs
// the hub reads (cron/jobs.py create_job and _record_run_outcome).
type cronJob struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	LastRunAt  json.RawMessage `json:"last_run_at"`
	LastStatus string          `json:"last_status"`
	LastError  string          `json:"last_error"`
	// DeliveryError is why the result could not be delivered.
	DeliveryError string `json:"last_delivery_error"`
}

// maxJobsBody bounds the job list the hub reads.
const maxJobsBody = 4 << 20

func (n *Notifier) pollJobs(ctx context.Context) {
	for id, r := range n.o.Jobs {
		if ctx.Err() != nil {
			return
		}
		jobs, err := readJobs(ctx, r)
		if err != nil {
			n.log.Debug("Failed to read scheduled jobs", "backend", id, "error", err.Error())
			continue
		}
		for _, j := range jobs {
			n.checkJob(ctx, id, j)
		}
	}
}

func readJobs(ctx context.Context, r Requester) ([]cronJob, error) {
	resp, err := r.Request(ctx, http.MethodGet, "/api/cron/jobs", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var jobs []cronJob
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJobsBody)).Decode(&jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// checkJob sends a notice for a job run the hub had not seen. A job it
// never saw before only sets the mark, so a hub start does not repeat old
// results.
func (n *Notifier) checkJob(ctx context.Context, backendID string, j cronJob) {
	mark := strings.TrimSpace(string(j.LastRunAt))
	if j.ID == "" || mark == "" || mark == "null" {
		return
	}
	prev, seen, err := n.o.Store.CronMark(ctx, backendID, j.ID)
	if err != nil || prev == mark {
		return
	}
	if err := n.o.Store.SetCronMark(ctx, backendID, j.ID, mark); err != nil || !seen {
		return
	}
	name := j.Name
	if name == "" {
		name = j.ID
	}
	nt := hub.Notice{BackendID: backendID, Key: "job:" + j.ID + ":" + mark}
	switch j.LastStatus {
	case "ok":
		nt.Kind, nt.Detail = hub.NoticeJob, "Scheduled job "+name+" finished."
	case "error":
		nt.Kind, nt.Detail = hub.NoticeFailure, jobFailure(name, j.LastError)
	case "delivery_failed":
		nt.Kind, nt.Detail = hub.NoticeFailure, "Scheduled job "+name+" ran, but its result could not be delivered: "+short(j.DeliveryError)
	default:
		return
	}
	n.handle(ctx, nt)
}

// loginWords are in errors of jobs that need the person to sign in again.
func loginWords() []string {
	return []string{"sign in", "signed in", "log in", "logged in", "login", "unauthorized", "401", "403",
		"expired token", "token expired", "reauth", "authenticate"}
}

func jobFailure(name, msg string) string {
	low := strings.ToLower(msg)
	for _, w := range loginWords() {
		if strings.Contains(low, w) {
			return "Scheduled job " + name + " needs you to sign in again: " + short(msg)
		}
	}
	return "Scheduled job " + name + " failed: " + short(msg)
}

// secretLike matches long runs of token characters, as API keys and
// tokens are, so a job's error does not carry one to a phone.
var secretLike = regexp.MustCompile(`[A-Za-z0-9_\-+/=.]{24,}`)

func short(s string) string {
	s = secretLike.ReplaceAllString(strings.Join(strings.Fields(s), " "), "[hidden]")
	if s == "" {
		return "no reason given."
	}
	if r := []rune(s); len(r) > 300 {
		return string(r[:300]) + "..."
	}
	return s
}
