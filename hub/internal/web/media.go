package web

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// handleMedia streams a file that the agent named with a MEDIA directive in
// the session: an image inline, any other file as a download. The service
// checks that the user may use the backend and that the session's replies
// name the path, and reads it from the backend's media roots or from the
// device that runs the session.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	m, ok := s.svc.(hub.Media)
	p := r.URL.Query().Get("path")
	if !ok || !hub.MediaPath(p) {
		errorJSON(w, http.StatusNotFound, hub.ErrNoMedia.Error())
		return
	}
	f, err := m.Media(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), p)
	switch {
	case errors.Is(err, hub.ErrNoMedia):
		errorJSON(w, http.StatusNotFound, hub.ErrNoMedia.Error())
		return
	case errors.Is(err, hub.ErrMediaTooLarge):
		errorJSON(w, http.StatusRequestEntityTooLarge, "the file is too large")
		return
	case errors.Is(err, hub.ErrMediaOffline):
		errorJSON(w, http.StatusServiceUnavailable, hub.ErrMediaOffline.Error())
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	defer f.Body.Close()
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	if f.NoStore {
		h.Set("Cache-Control", "no-store")
	} else {
		h.Set("Cache-Control", "private, max-age=300")
	}
	disposition := "attachment"
	if f.MIME != "" && r.URL.Query().Get("download") == "" {
		h.Set("Content-Type", f.MIME)
		disposition = "inline"
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": f.Name}))
	if f.Size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, f.Body); err != nil {
		s.log.Debug("media copy ended", "error", err.Error())
	}
}
