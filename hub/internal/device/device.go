// Package device is the hub side of hermes-device (docs/protocol.md of
// github.com/USA-RedDragon/hermes-device): pairing, the device token, the
// bind API and the exec, ACP and gateway legs that the hub proxies to a
// device's backend.
package device

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-device/protocol"
	"github.com/USA-RedDragon/hermes-hub/internal/config"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
	"github.com/coder/websocket"
)

// Error codes of the hub's device API.
const (
	codeUnauthorized       = "unauthorized"
	codeBackendUnavailable = "backend_unavailable"
	codeForbidden          = "forbidden"
	codeBadRequest         = "bad_request"
	codeRateLimited        = "rate_limited"
	codeInternal           = "internal_error"
)

// JSON member names of the device API.
const (
	keyDeviceID   = "device_id"
	keyDeviceName = "device_name"
	keyBackend    = "backend"
)

// Access decides whether a user may use a backend. It fails with
// hub.ErrForbidden or hub.ErrUnknownBackend when not.
type Access interface {
	Allowed(ctx context.Context, user, backendID string) error
}

// Dialer opens connections to one backend's gateway listener.
// *gateway.Dialer is one.
type Dialer interface {
	Requester
	DialRaw(ctx context.Context) (*websocket.Conn, error)
}

// Options are the dependencies of a Server.
type Options struct {
	Store  hub.DeviceStore
	Access Access
	// Sessions starts the sessions the hub starts on computers. Without
	// it StartOn fails.
	Sessions Sessions
	Backends []config.Backend
	// Dialers by backend id. A backend without one gets none of the legs.
	Dialers map[string]Dialer
	Key     ed25519.PrivateKey
	// PublicURL is the hub's base URL, for the pairing link.
	PublicURL string
	Config    config.Device
	Logger    *slog.Logger
	// PingInterval and PingTimeout default to the protocol's.
	PingInterval, PingTimeout time.Duration
	// SeenInterval is how often a device's last seen time is written while
	// its address stays the same. Default 30s.
	SeenInterval time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

type backend struct {
	cfg    config.Backend
	dialer Dialer
	plugin *Plugin

	mu          sync.Mutex
	placeholder string
	phAt        time.Time

	// reads holds a slot for each file read from a device that waits for
	// the plugin's answer.
	reads chan struct{}
}

// Server serves the device side of the hub.
type Server struct {
	opts     Options
	log      *slog.Logger
	store    hub.DeviceStore
	backends map[string]*backend
	proxies  []netip.Prefix
	pairs    *limiter
	polls    *limiter

	// service maps the SHA-256 of each service device token to its device.
	service map[[32]byte]hub.Device
	// resolved caches the plugin's resolution of sessions without a
	// binding of their own, for Places.
	resolved resolvedCache

	mu    sync.Mutex
	legs  map[string]map[*leg]struct{}
	binds map[string]bindEntry
	// created are the sessions a leg of a device created for it, by
	// device id and session id, with when.
	created map[string]time.Time
	closed  bool
	stopped chan struct{}
}

// New returns a Server.
func New(opts Options) (*Server, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.PingInterval == 0 {
		opts.PingInterval = protocol.PingInterval
	}
	if opts.PingTimeout == 0 {
		opts.PingTimeout = protocol.PingTimeout
	}
	if opts.SeenInterval == 0 {
		opts.SeenInterval = 30 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Config.RecheckInterval == 0 {
		opts.Config.RecheckInterval = 10 * time.Second
	}
	if opts.Config.PairingTTL == 0 {
		opts.Config.PairingTTL = 10 * time.Minute
	}
	if opts.Config.PollInterval == 0 {
		opts.Config.PollInterval = 5 * time.Second
	}
	if len(opts.Key) != ed25519.PrivateKeySize {
		return nil, errors.New("device: no Ed25519 signing key")
	}
	proxies, err := opts.Config.Proxies()
	if err != nil {
		return nil, err
	}
	s := &Server{opts: opts, log: opts.Logger.With("component", "device"), store: opts.Store, backends: map[string]*backend{},
		proxies: proxies, pairs: newLimiter(20, 10*time.Minute), polls: newLimiter(400, 10*time.Minute),
		legs: map[string]map[*leg]struct{}{}, binds: map[string]bindEntry{}, created: map[string]time.Time{}, stopped: make(chan struct{}),
		service: map[[32]byte]hub.Device{}}
	for _, b := range opts.Backends {
		d := opts.Dialers[b.ID]
		if d == nil {
			continue
		}
		s.backends[b.ID] = &backend{cfg: b, dialer: d, plugin: NewPlugin(d), reads: make(chan struct{}, maxReads)}
		for _, sd := range b.ServiceDevices {
			s.service[sha256.Sum256([]byte(sd.Token))] = hub.Device{ID: sd.ID, User: hub.ServiceUser, BackendID: b.ID,
				Name: sd.Name, Paired: true, Service: true}
		}
	}
	return s, nil
}

// Handler serves /api/device/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/device/pair", s.handlePair)
	mux.HandleFunc("POST /api/device/pair/token", s.handlePairToken)
	mux.Handle("GET /api/device/whoami", s.authed(s.handleWhoami))
	mux.Handle("PUT /api/device/sessions/{session}/binding", s.authed(s.exceptService(s.handleBind)))
	mux.Handle("DELETE /api/device/sessions/{session}/binding", s.authed(s.exceptService(s.handleUnbind)))
	mux.Handle("GET /api/device/exec", s.authed(s.handleExec))
	mux.Handle("GET /api/device/acp", s.authed(s.exceptService(s.handleACP)))
	mux.Handle("GET /api/device/gateway", s.authed(s.exceptService(s.handleGateway)))
	mux.HandleFunc("/api/device/", func(w http.ResponseWriter, _ *http.Request) {
		apiError(w, http.StatusNotFound, "not_found", "no such device API")
	})
	return mux
}

// Plugin returns the device plugin client of a backend, or nil.
func (s *Server) Plugin(backendID string) *Plugin {
	if b := s.backends[backendID]; b != nil {
		return b.plugin
	}
	return nil
}

// Drain closes every device connection with 1001, so the devices
// reconnect, to another replica when there is one. Later upgrades are
// refused.
func (s *Server) Drain() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.stopped)
	var all []*leg
	for _, set := range s.legs {
		for l := range set {
			all = append(all, l)
		}
	}
	s.mu.Unlock()
	for _, l := range all {
		l.finish(hub.CloseByHub, websocket.StatusGoingAway, websocket.StatusGoingAway, "hub shutting down")
	}
}

// CloseDevice closes every connection of a device on this replica with
// 4401, as after a revoke.
func (s *Server) CloseDevice(id string) {
	s.mu.Lock()
	ls := make([]*leg, 0, len(s.legs[id]))
	for l := range s.legs[id] {
		ls = append(ls, l)
	}
	s.mu.Unlock()
	for _, l := range ls {
		l.finish(hub.CloseByHub, protocol.CloseUnauthorized, websocket.StatusNormalClosure, "device revoked")
	}
}

// recordLeg runs a store write about a leg of device id and logs a
// failure: the computer list is only a display.
func (s *Server) recordLeg(ctx context.Context, id string, fn func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	if err := fn(ctx); err != nil {
		s.log.Warn("record exec leg", "device", id, "error", err.Error())
	}
}

const recordTimeout = 10 * time.Second

type deviceKey struct{}

func deviceFrom(r *http.Request) hub.Device {
	d, _ := r.Context().Value(deviceKey{}).(hub.Device)
	return d
}

func apiError(w http.ResponseWriter, status int, code, message string) {
	body := map[string]string{"error": code}
	if message != "" {
		body["message"] = message
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("write device API answer", "error", err.Error())
	}
}

func writeRaw(w http.ResponseWriter, status int, body []byte) {
	if status != http.StatusNoContent {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_, _ = w.Write(body)
	}
}

func decodeBody(r *http.Request, v any) bool {
	return json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(v) == nil
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// authed checks the device token, that the device's backend still exists
// and that its user may still use it, and records the request.
func (s *Server) authed(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			apiError(w, http.StatusUnauthorized, codeUnauthorized, "a device token is required")
			return
		}
		if d, ok := s.serviceDevice(tok); ok {
			h(w, r.WithContext(context.WithValue(r.Context(), deviceKey{}, d)))
			return
		}
		d, ok, err := s.store.DeviceByToken(r.Context(), tok)
		if err != nil {
			s.log.Error("look up device token", "error", err.Error())
			apiError(w, http.StatusServiceUnavailable, codeInternal, "the hub could not check the token")
			return
		}
		if !ok {
			apiError(w, http.StatusUnauthorized, codeUnauthorized, "unknown or revoked device token")
			return
		}
		if err := s.allowed(r.Context(), d); err != nil {
			if errors.Is(err, hub.ErrForbidden) || errors.Is(err, hub.ErrUnknownBackend) {
				s.log.Warn("device refused: its backend is not allowed for its user", "device", d.ID, "user", d.User,
					"backend", d.BackendID)
				apiError(w, http.StatusUnauthorized, codeUnauthorized, "this device's Hermes is no longer available to its user")
				return
			}
			s.log.Error("check device backend", "device", d.ID, "error", err.Error())
			apiError(w, http.StatusServiceUnavailable, codeInternal, "the hub could not check the device's access")
			return
		}
		s.seen(r.Context(), d, s.clientAddr(r))
		h(w, r.WithContext(context.WithValue(r.Context(), deviceKey{}, d)))
	})
}

// serviceDevice finds the service device of a token, comparing hashes in
// constant time.
func (s *Server) serviceDevice(tok string) (hub.Device, bool) {
	sum := sha256.Sum256([]byte(tok))
	var found hub.Device
	ok := false
	for h, d := range s.service {
		if subtle.ConstantTimeCompare(h[:], sum[:]) == 1 {
			found, ok = d, true
		}
	}
	return found, ok
}

// exceptService refuses service devices: they may use only the exec leg.
func (s *Server) exceptService(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deviceFrom(r).Service {
			apiError(w, http.StatusForbidden, codeForbidden, "a sandbox device may use only the exec leg")
			return
		}
		h(w, r)
	}
}

// Sandboxes lists the service devices of the backends user may use, and
// whether each is connected.
func (s *Server) Sandboxes(ctx context.Context, user string) []hub.Sandbox {
	out := []hub.Sandbox{}
	for _, b := range s.opts.Backends {
		sb := s.backends[b.ID]
		if sb == nil || len(b.ServiceDevices) == 0 || s.opts.Access.Allowed(ctx, user, b.ID) != nil {
			continue
		}
		var online map[string]bool
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		conns, err := sb.plugin.Devices(cctx)
		cancel()
		if err == nil {
			online = map[string]bool{}
			for _, c := range conns {
				online[c.DeviceID] = true
			}
		}
		for _, sd := range b.ServiceDevices {
			x := hub.Sandbox{ID: sd.ID, Name: sd.Name, BackendID: b.ID}
			if online != nil {
				on := online[sd.ID]
				x.Online = &on
			}
			out = append(out, x)
		}
	}
	return out
}

func (s *Server) allowed(ctx context.Context, d hub.Device) error {
	if s.backends[d.BackendID] == nil {
		return hub.ErrUnknownBackend
	}
	return s.opts.Access.Allowed(ctx, d.User, d.BackendID)
}

func (s *Server) seen(ctx context.Context, d hub.Device, addr string) {
	if addr == d.LastAddr && s.opts.Now().Sub(d.LastSeenAt) < s.opts.SeenInterval {
		return
	}
	if err := s.store.SeenDevice(ctx, d.ID, addr); err != nil {
		s.log.Warn("record device request", "device", d.ID, "error", err.Error())
		return
	}
	if d.LastAddr != "" && addr != d.LastAddr {
		s.log.Info("device connected from a new address", "device", d.ID, "user", d.User, "previous", d.LastAddr, "address", addr)
	}
}

// clientAddr is the request's peer address, or for a peer in
// device.trusted_proxies the last address in X-Forwarded-For that is not
// a trusted proxy.
func (s *Server) clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if !s.trusted(peer) {
		return peer.String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			return peer.String()
		}
		a = a.Unmap()
		if !s.trusted(a) {
			return a.String()
		}
		if i == 0 {
			return a.String()
		}
	}
	return peer.String()
}

func (s *Server) trusted(a netip.Addr) bool {
	for _, p := range s.proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// limiter counts requests per key in fixed windows.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func newLimiter(maxHits int, w time.Duration) *limiter {
	return &limiter{max: maxHits, window: w, hits: map[string]*window{}}
}

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 10000 {
		for k, w := range l.hits {
			if now.Sub(w.start) >= l.window {
				delete(l.hits, k)
			}
		}
	}
	w := l.hits[key]
	if w == nil || now.Sub(w.start) >= l.window {
		w = &window{start: now}
		l.hits[key] = w
	}
	w.n++
	return w.n <= l.max
}
