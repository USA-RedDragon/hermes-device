package device

import (
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// PairPath is the hub page where a signed-in user types a pairing code.
const PairPath = "/device/pair"

// MaxNameLen is the longest device name in bytes, the limit of the
// assertion's device_name claim.
const MaxNameLen = 64

// CleanName trims a device name and checks it: 1 to MaxNameLen bytes of
// valid UTF-8 with no control characters.
func CleanName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > MaxNameLen || !utf8.ValidString(s) {
		return s, false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return s, false
		}
	}
	return s, true
}

// clip makes a display field safe to store: valid UTF-8, no control
// characters, at most n bytes.
func clip(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	now := s.opts.Now()
	if !s.pairs.allow(s.clientAddr(r), now) {
		apiError(w, http.StatusTooManyRequests, codeRateLimited, "too many pairing requests from this address; try again later")
		return
	}
	var body struct {
		Name          string `json:"name"`
		Hostname      string `json:"hostname"`
		OS            string `json:"os"`
		Arch          string `json:"arch"`
		ClientVersion string `json:"client_version"`
	}
	if !decodeBody(r, &body) {
		apiError(w, http.StatusBadRequest, codeBadRequest, "the body must be a JSON object")
		return
	}
	req := hub.PairingRequest{Name: clip(body.Name, MaxNameLen), Hostname: clip(body.Hostname, 255), OS: clip(body.OS, 32),
		Arch: clip(body.Arch, 32), ClientVersion: clip(body.ClientVersion, 64), Addr: s.clientAddr(r)}
	if req.Name == "" {
		req.Name = clip(req.Hostname, MaxNameLen)
	}
	code, userCode, err := s.store.StartPairing(r.Context(), req, s.opts.Config.PairingTTL)
	if err != nil {
		s.log.Error("start pairing", "error", err.Error())
		apiError(w, http.StatusInternalServerError, codeInternal, "")
		return
	}
	s.log.Info("pairing requested", "hostname", req.Hostname, "address", req.Addr)
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code": code, "user_code": userCode, "verification_uri": s.opts.PublicURL + PairPath,
		"expires_in": int(s.opts.Config.PairingTTL / time.Second), "interval": int(s.opts.Config.PollInterval / time.Second),
	})
}

func (s *Server) handlePairToken(w http.ResponseWriter, r *http.Request) {
	if !s.polls.allow(s.clientAddr(r), s.opts.Now()) {
		apiError(w, http.StatusBadRequest, hub.PairSlowDown, "")
		return
	}
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if !decodeBody(r, &body) || body.DeviceCode == "" {
		apiError(w, http.StatusBadRequest, "invalid_request", `the body must be {"device_code": <string>}`)
		return
	}
	res, err := s.store.PollPairing(r.Context(), body.DeviceCode, s.opts.Config.PollInterval)
	if err != nil {
		s.log.Error("poll pairing", "error", err.Error())
		apiError(w, http.StatusInternalServerError, codeInternal, "")
		return
	}
	if res.Status != hub.PairApproved {
		apiError(w, http.StatusBadRequest, res.Status, "")
		return
	}
	s.log.Info("device paired", "device", res.Device.ID, "user", res.Device.User, "backend", res.Device.BackendID,
		"name", res.Device.Name)
	writeJSON(w, http.StatusOK, map[string]string{"token": res.Token, keyDeviceID: res.Device.ID,
		keyDeviceName: res.Device.Name, keyBackend: res.Device.BackendID})
}
