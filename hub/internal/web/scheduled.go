package web

import (
	"errors"
	"net/http"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// scheduledRoutes adds the Scheduled page's API. Like every route but GET,
// the changes need the CSRF token and a same-origin request.
func (s *Server) scheduledRoutes(mux *http.ServeMux) {
	const base = "/api/scheduled/{backend}"
	mux.Handle("GET /api/scheduled", s.api(s.handleJobs))
	mux.Handle("GET "+base+"/options", s.api(s.handleJobOptions))
	mux.Handle("POST "+base+"/preview", s.api(s.handlePreviewSchedule))
	mux.Handle("DELETE "+base+"/drafts/{draft}", s.api(s.handleDiscardDraft))
	mux.Handle("POST "+base+"/jobs", s.api(s.handleCreateJob))
	mux.Handle("PUT "+base+"/jobs/{job}", s.api(s.handleUpdateJob))
	mux.Handle("DELETE "+base+"/jobs/{job}", s.api(s.handleDeleteJob))
	mux.Handle("GET "+base+"/jobs/{job}/runs", s.api(s.handleJobRuns))
	mux.Handle("POST "+base+"/jobs/{job}/{verb}", s.api(s.handleJobAction))
}

// scheduledFail answers a hub.Scheduled error.
func (s *Server) scheduledFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hub.ErrNoScheduled):
		errorJSON(w, http.StatusNotFound, hub.ErrNoScheduled.Error())
	case errors.Is(err, hub.ErrUnknownJob):
		errorJSON(w, http.StatusNotFound, hub.ErrUnknownJob.Error())
	case errors.Is(err, hub.ErrJobLocked):
		errorJSON(w, http.StatusConflict, hub.ErrJobLocked.Error())
	default:
		s.fail(w, err)
	}
}

func (s *Server) noScheduled(w http.ResponseWriter) bool {
	if s.scheduled == nil {
		errorJSON(w, http.StatusNotFound, hub.ErrNoScheduled.Error())
		return true
	}
	return false
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if s.noScheduled(w) {
		return
	}
	out, err := s.scheduled.Jobs(r.Context(), userFrom(r))
	if err != nil {
		s.scheduledFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleJobOptions(w http.ResponseWriter, r *http.Request) {
	if s.noScheduled(w) {
		return
	}
	out, err := s.scheduled.JobOptions(r.Context(), userFrom(r), r.PathValue("backend"))
	if err != nil {
		s.scheduledFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePreviewSchedule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Schedule string `json:"schedule"`
		Draft    string `json:"draft"`
	}
	if !decode(w, r, &body) || s.noScheduled(w) {
		return
	}
	out, err := s.scheduled.PreviewSchedule(r.Context(), userFrom(r), r.PathValue("backend"), body.Schedule, body.Draft)
	if err != nil {
		s.scheduledFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDiscardDraft(w http.ResponseWriter, r *http.Request) {
	if s.noScheduled(w) {
		return
	}
	if err := s.scheduled.DiscardDraft(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("draft")); err != nil {
		s.scheduledFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var in hub.JobInput
	if !decode(w, r, &in) || s.noScheduled(w) {
		return
	}
	j, err := s.scheduled.CreateJob(r.Context(), userFrom(r), r.PathValue("backend"), in)
	if err != nil {
		s.scheduledFail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, j)
}

func (s *Server) handleUpdateJob(w http.ResponseWriter, r *http.Request) {
	var in hub.JobInput
	if !decode(w, r, &in) || s.noScheduled(w) {
		return
	}
	j, err := s.scheduled.UpdateJob(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("job"), in)
	if err != nil {
		s.scheduledFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	if s.noScheduled(w) {
		return
	}
	if err := s.scheduled.DeleteJob(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("job")); err != nil {
		s.scheduledFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleJobRuns(w http.ResponseWriter, r *http.Request) {
	if s.noScheduled(w) {
		return
	}
	runs, err := s.scheduled.JobRuns(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("job"))
	if err != nil {
		s.scheduledFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleJobAction pauses, resumes, runs or stops a job.
func (s *Server) handleJobAction(w http.ResponseWriter, r *http.Request) {
	if s.noScheduled(w) {
		return
	}
	user, backend, job := userFrom(r), r.PathValue("backend"), r.PathValue("job")
	ctx := r.Context()
	switch r.PathValue("verb") {
	case "pause", "resume":
		act := s.scheduled.PauseJob
		if r.PathValue("verb") == "resume" {
			act = s.scheduled.ResumeJob
		}
		j, err := act(ctx, user, backend, job)
		if err != nil {
			s.scheduledFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, j)
	case "run":
		if err := s.scheduled.RunJob(ctx, user, backend, job); err != nil {
			s.scheduledFail(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
	case "cancel":
		ok, err := s.scheduled.CancelJob(ctx, user, backend, job)
		if err != nil {
			s.scheduledFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"cancelled": ok})
	default:
		errorJSON(w, http.StatusNotFound, "not found")
	}
}
