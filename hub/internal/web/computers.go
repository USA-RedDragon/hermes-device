package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// maxPlaces is how many sessions one places request may ask about.
const maxPlaces = 500

const msgNoComputers = "computers are not turned on in this hub"

type computerJSON struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	BackendID   string   `json:"backend_id"`
	BackendName string   `json:"backend_name"`
	Online      bool     `json:"online"`
	Hostname    string   `json:"hostname"`
	OS          string   `json:"os"`
	Home        string   `json:"home,omitempty"`
	Roots       []string `json:"roots"`
	Recent      []string `json:"recent"`
}

func (s *Server) backendName(id string) string {
	if n := s.backendNames[id]; n != "" {
		return n
	}
	return id
}

func (s *Server) handleComputers(w http.ResponseWriter, r *http.Request) {
	if s.computers == nil {
		writeJSON(w, http.StatusOK, []computerJSON{})
		return
	}
	cs, err := s.computers.Computers(r.Context(), userFrom(r))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]computerJSON, 0, len(cs))
	for _, c := range cs {
		j := computerJSON{ID: c.ID, Name: c.Name, BackendID: c.BackendID, BackendName: s.backendName(c.BackendID), Online: c.Online,
			Hostname: c.Hostname, OS: c.OS, Home: c.Home, Roots: c.Roots, Recent: c.Recent}
		if j.Roots == nil {
			j.Roots = []string{}
		}
		if j.Recent == nil {
			j.Recent = []string{}
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	if s.computers == nil {
		errorJSON(w, http.StatusNotFound, msgNoComputers)
		return
	}
	f, err := s.computers.Folders(r.Context(), userFrom(r), r.PathValue("device"), r.URL.Query().Get("path"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if f.Dirs == nil {
		f.Dirs = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": f.Path, "folders": f.Dirs})
}

type placeJSON struct {
	hub.SessionRef
	Place hub.Place `json:"place"`
}

func (s *Server) handlePlaces(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Sessions []hub.SessionRef `json:"sessions"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Sessions) > maxPlaces {
		body.Sessions = body.Sessions[:maxPlaces]
	}
	out := []placeJSON{}
	if s.computers != nil {
		for k, p := range s.computers.Places(r.Context(), userFrom(r), body.Sessions) {
			out = append(out, placeJSON{SessionRef: k, Place: p})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePlace(w http.ResponseWriter, r *http.Request) {
	if s.computers == nil {
		writeJSON(w, http.StatusOK, hub.Place{})
		return
	}
	p, err := s.computers.Place(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceID string `json:"device_id"`
		Dir      string `json:"dir"`
	}
	if !decode(w, r, &body) {
		return
	}
	if s.computers == nil {
		errorJSON(w, http.StatusNotFound, msgNoComputers)
		return
	}
	p, err := s.computers.MoveTo(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session"), body.DeviceID, body.Dir)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	if s.computers == nil {
		errorJSON(w, http.StatusNotFound, msgNoComputers)
		return
	}
	s.done(w, s.computers.Release(r.Context(), userFrom(r), r.PathValue("backend"), r.PathValue("session")))
}

// startOn starts a chat on a computer for handleNewSession.
func (s *Server) startOn(w http.ResponseWriter, r *http.Request, deviceID, dir string) {
	if s.computers == nil {
		errorJSON(w, http.StatusNotFound, msgNoComputers)
		return
	}
	sess, p, err := s.computers.StartOn(r.Context(), userFrom(r), deviceID, dir)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		sessionJSON
		Place hub.Place `json:"place"`
	}{toJSON(sess), p})
}

// computerError writes the answer to a computer error, and reports
// whether err was one.
func computerError(w http.ResponseWriter, err error) bool {
	reason := func() string {
		_, msg, _ := strings.Cut(err.Error(), ": ")
		if msg == "" {
			return "Hermes did not accept that"
		}
		return strings.ToUpper(msg[:1]) + msg[1:]
	}
	switch {
	case errors.Is(err, hub.ErrUnknownDevice):
		errorJSON(w, http.StatusNotFound, "No such computer. It may have been removed")
	case errors.Is(err, hub.ErrComputerOffline):
		errorJSON(w, http.StatusConflict, "That computer is offline. Turn it on and check that hermes-device runs on it, then try again")
	case errors.Is(err, hub.ErrFolderRefused):
		errorJSON(w, http.StatusBadRequest, reason())
	case errors.Is(err, hub.ErrCannotMove):
		errorJSON(w, http.StatusConflict, reason())
	case errors.Is(err, hub.ErrOtherBackend):
		errorJSON(w, http.StatusBadRequest, "That computer works with another Hermes. Pick a computer of this chat's Hermes")
	case errors.Is(err, hub.ErrNotOnComputer):
		errorJSON(w, http.StatusConflict, "This chat is not on a computer")
	case errors.Is(err, hub.ErrInheritedPlace):
		errorJSON(w, http.StatusConflict, "This chat uses the computer of the chat it came from. Move it to a computer to change that")
	default:
		return false
	}
	return true
}
