package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// maxViewReport bounds a view report's body.
const maxViewReport = 16 << 10

// viewReportRoute lets a beacon in: it cannot set the CSRF header, so its
// token may be the body's "csrf" field. The page's checks are the same.
func (s *Server) viewReportRoute() http.Handler {
	inner := s.api(s.handleViewReport)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-CSRF-Token") == "" && r.Body != nil {
			body, err := io.ReadAll(io.LimitReader(r.Body, maxViewReport+1))
			if err == nil && len(body) <= maxViewReport {
				var f struct {
					CSRF string `json:"csrf"`
				}
				if json.Unmarshal(body, &f) == nil {
					r.Header.Set("X-CSRF-Token", f.CSRF)
				}
			}
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
		}
		inner.ServeHTTP(w, r)
	})
}

// handleViewReport logs what a page reports: that it loaded and with which
// release, or that the chat it shows left the newest message without the
// reader doing anything (the log's last moves, the chat's events, the
// page's age). Nothing is stored; it is for finding what moves the view in
// a browser the tests do not catch. The token a beacon carries is dropped.
func (s *Server) handleViewReport(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Content-Type") != "application/json" {
		errorJSON(w, http.StatusUnsupportedMediaType, "JSON body required")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxViewReport+1))
	if err != nil || len(body) > maxViewReport || !json.Valid(body) {
		errorJSON(w, http.StatusBadRequest, "bad report")
		return
	}
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		delete(m, "csrf")
		if clean, err := json.Marshal(m); err == nil {
			body = clean
		}
	}
	msg := "view left the newest message"
	if m["kind"] == "loaded" {
		msg = "page loaded"
	}
	s.log.Info(msg, "user", userFrom(r), "ua", r.UserAgent(), "report", string(body))
	w.WriteHeader(http.StatusNoContent)
}
