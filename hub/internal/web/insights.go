package web

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// insightRoutes adds chat search, the Usage page and the file viewer.
func (s *Server) insightRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/search", s.api(s.handleSearch))
	mux.Handle("GET /api/usage/{backend}", s.api(s.handleUsage))
	mux.Handle("GET /api/sessions/{backend}/{session}/folder", s.api(s.handleFolder))
	mux.Handle("GET /api/sessions/{backend}/{session}/file", s.api(s.handleFile))
	mux.Handle("GET /api/sessions/{backend}/{session}/diff", s.api(s.handleDiff))
	mux.Handle("GET /api/sessions/{backend}/{session}/changes", s.api(s.handleChanges))
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.insights == nil {
		errorJSON(w, http.StatusNotFound, "search is not set up")
		return
	}
	out, err := s.insights.Search(r.Context(), userFrom(r), r.URL.Query().Get("q"))
	if errors.Is(err, hub.ErrSearchBusy) {
		errorJSON(w, http.StatusTooManyRequests, hub.ErrSearchBusy.Error())
		return
	}
	if errors.Is(err, hub.ErrBadQuery) {
		_, msg, _ := strings.Cut(err.Error(), ": ")
		errorJSON(w, http.StatusBadRequest, msg)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if s.insights == nil {
		errorJSON(w, http.StatusNotFound, "the Usage page is not set up")
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 30
	}
	out, err := s.insights.Usage(r.Context(), userFrom(r), r.PathValue("backend"), days)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// filesFail answers a ChatFiles error.
func (s *Server) filesFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hub.ErrNoFolder):
		errorJSON(w, http.StatusNotFound, "This chat does not run on one of your computers or in the sandbox, so it has no folder to show")
	case errors.Is(err, hub.ErrFilesTooOld):
		errorJSON(w, http.StatusNotFound, "The Talaria device plugin is too old for this. Update talaria on the Hermes server")
	case errors.Is(err, hub.ErrFileNotFound):
		errorJSON(w, http.StatusNotFound, "Not found. It may have been moved or deleted")
	case errors.Is(err, hub.ErrFileRefused):
		_, msg, _ := strings.Cut(err.Error(), ": ")
		errorJSON(w, http.StatusForbidden, "The computer does not show this: "+msg)
	case errors.Is(err, hub.ErrMediaOffline):
		errorJSON(w, http.StatusServiceUnavailable, "The computer is offline")
	case errors.Is(err, hub.ErrMediaTooLarge):
		errorJSON(w, http.StatusRequestEntityTooLarge, "The file is too large to open here")
	case errors.Is(err, hub.ErrFilesBusy):
		errorJSON(w, http.StatusTooManyRequests, hub.ErrFilesBusy.Error())
	default:
		s.fail(w, err)
	}
}

func (s *Server) handleFolder(w http.ResponseWriter, r *http.Request) {
	if s.files == nil {
		s.filesFail(w, hub.ErrNoFolder)
		return
	}
	out, err := s.files.ChatFolder(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), r.URL.Query().Get("path"))
	if err != nil {
		s.filesFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFile sends a file of a chat's folder as a download: never as a
// page, whatever it holds. The web UI reads it with fetch.
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	if s.files == nil {
		s.filesFail(w, hub.ErrNoFolder)
		return
	}
	f, err := s.files.ChatFile(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), r.URL.Query().Get("path"))
	if err != nil {
		s.filesFail(w, err)
		return
	}
	defer f.Body.Close()
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
	if f.Size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f.Body); err != nil {
		s.log.Debug("file copy ended", "error", err.Error())
	}
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	if s.files == nil {
		s.filesFail(w, hub.ErrNoFolder)
		return
	}
	out, err := s.files.ChatDiff(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"))
	if err != nil {
		s.filesFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request) {
	if s.files == nil {
		s.filesFail(w, hub.ErrNoFolder)
		return
	}
	out, err := s.files.ChatChanges(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"))
	if err != nil {
		s.filesFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
