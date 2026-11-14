package web

import (
	"encoding/json"
	"io"
	"net/http"
)

// maxViewReport bounds a view report's body.
const maxViewReport = 16 << 10

// handleViewReport logs what a page reports when a chat it opened left
// the newest message without the reader doing anything: the log's last
// moves and the chat's events since the open. Nothing is stored; it is
// for finding what moves the view in a browser the tests do not catch.
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
	s.log.Info("view left the newest message after an open", "user", userFrom(r), "ua", r.UserAgent(), "report", string(body))
	w.WriteHeader(http.StatusNoContent)
}
