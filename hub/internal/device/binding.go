package device

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// placeholderTTL is how long a backend's placeholder directory is cached
// for the gateway leg.
const placeholderTTL = time.Minute

const pluginTimeout = 20 * time.Second

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	d := deviceFrom(r)
	b := s.backends[d.BackendID]
	ctx, cancel := context.WithTimeout(r.Context(), pluginTimeout)
	defer cancel()
	info, err := b.plugin.Info(ctx)
	if err != nil {
		s.log.Warn("device plugin info", "backend", d.BackendID, "error", err.Error())
		apiError(w, http.StatusBadGateway, codeBackendUnavailable, "the hub could not reach the device plugin of this device's Hermes")
		return
	}
	b.setPlaceholder(info.Placeholder, s.opts.Now())
	writeJSON(w, http.StatusOK, map[string]string{
		keyDeviceID: d.ID, keyDeviceName: d.Name, "user": d.User, keyBackend: d.BackendID,
		"hermes_version": info.HermesVersion, "plugin_version": info.PluginVersion, "placeholder": info.Placeholder,
	})
}

func (b *backend) setPlaceholder(p string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.placeholder, b.phAt = p, now
}

func (b *backend) forgetPlaceholder() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.placeholder = ""
}

// placeholderDir is the backend's placeholder cwd, from the plugin's /info.
func (s *Server) placeholderDir(ctx context.Context, b *backend) (string, error) {
	now := s.opts.Now()
	b.mu.Lock()
	p, at := b.placeholder, b.phAt
	b.mu.Unlock()
	if p != "" && now.Sub(at) < placeholderTTL {
		return p, nil
	}
	ctx, cancel := context.WithTimeout(ctx, pluginTimeout)
	defer cancel()
	info, err := b.plugin.Info(ctx)
	if err != nil {
		return "", err
	}
	if info.Placeholder == "" {
		return "", errors.New("the device plugin reports no placeholder")
	}
	b.setPlaceholder(info.Placeholder, now)
	return info.Placeholder, nil
}

// errWasBound is a session with no binding that it or an ancestor once
// had, or whose lineage the plugin cannot read.
var errWasBound = errors.New("the session was bound to a device and is not now, or its lineage cannot be read")

// noteCreated remembers that a leg of a device created sid for it.
func (s *Server) noteCreated(deviceID, sid string) {
	now := s.opts.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.created) > 4096 {
		for k, at := range s.created {
			if now.Sub(at) >= bindCacheTTL {
				delete(s.created, k)
			}
		}
	}
	s.created[deviceID+"\x00"+sid] = now
}

func (s *Server) createdBy(deviceID, sid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.created[deviceID+"\x00"+sid]
	return ok && s.opts.Now().Sub(at) < bindCacheTTL
}

// bindAllowed refuses a bind of sid to a device unless the plugin resolves
// the session (through its lineage) to that device, or to no device with
// none in its lineage ever bound. A session that fails closed passes only
// when a leg of this hub created it for the device within bindCacheTTL,
// such as a branch child whose lineage row is not written yet.
func (s *Server) bindAllowed(ctx context.Context, b *backend, deviceID, sid string) error {
	res, err := b.plugin.Resolve(ctx, sid)
	if err != nil {
		return err
	}
	switch res.Resolved.State {
	case StateBound:
		if res.DeviceID != deviceID {
			return errOtherDevice
		}
		return nil
	case StateNeverBound:
		return nil
	}
	if s.createdBy(deviceID, sid) {
		return nil
	}
	return errWasBound
}

// neverBound reports whether the plugin resolves sid to no binding, with
// none in its lineage ever bound.
func (s *Server) neverBound(ctx context.Context, b *backend, sid string) (bool, error) {
	res, err := b.plugin.Resolve(ctx, sid)
	if err != nil {
		return false, err
	}
	return res.Resolved.State == StateNeverBound, nil
}

// handleBind forwards a bind to the plugin with the token's device id, and
// passes the plugin's answer through. It refuses a session that bindAllowed
// refuses, whatever the plugin would do.
func (s *Server) handleBind(w http.ResponseWriter, r *http.Request) {
	d := deviceFrom(r)
	var body struct {
		Dir *string `json:"dir"`
	}
	if !decodeBody(r, &body) || body.Dir == nil {
		apiError(w, http.StatusBadRequest, codeBadRequest, `the body must be {"dir": <string>}`)
		return
	}
	sid := r.PathValue("session")
	b := s.backends[d.BackendID]
	ctx, cancel := context.WithTimeout(r.Context(), pluginTimeout)
	defer cancel()
	switch err := s.bindAllowed(ctx, b, d.ID, sid); {
	case errors.Is(err, errOtherDevice):
		s.log.Warn("refused a bind of another device's session", "device", d.ID, "session_id", sid)
		apiError(w, http.StatusConflict, "bound_to_other_device", "the session is bound to another device (through its lineage)")
		return
	case errors.Is(err, errWasBound):
		s.log.Warn("refused a bind of a session that was bound before", "device", d.ID, "session_id", sid)
		apiError(w, http.StatusConflict, "was_bound", errWasBound.Error())
		return
	case err != nil:
		s.log.Warn("resolve binding before bind", "device", d.ID, "error", err.Error())
		apiError(w, http.StatusBadGateway, codeBackendUnavailable, "the hub could not reach the device plugin")
		return
	}
	status, data, err := b.plugin.Raw(ctx, http.MethodPut, sessionPath(sid),
		map[string]string{keyDeviceID: d.ID, "dir": *body.Dir})
	if err != nil {
		s.log.Warn("forward bind", "device", d.ID, "error", err.Error())
		apiError(w, http.StatusBadGateway, codeBackendUnavailable, "the hub could not reach the device plugin")
		return
	}
	writeRaw(w, status, data)
}

// handleUnbind forwards an unbind only when the session's own binding
// belongs to the token's device.
func (s *Server) handleUnbind(w http.ResponseWriter, r *http.Request) {
	d := deviceFrom(r)
	sid := r.PathValue("session")
	p := s.backends[d.BackendID].plugin
	ctx, cancel := context.WithTimeout(r.Context(), pluginTimeout)
	defer cancel()
	b, ok, err := p.Binding(ctx, sid)
	if err != nil {
		s.log.Warn("look up binding before unbind", "device", d.ID, "error", err.Error())
		apiError(w, http.StatusBadGateway, codeBackendUnavailable, "the hub could not reach the device plugin")
		return
	}
	if !ok || b.DeviceID != d.ID || b.SessionID != sid {
		apiError(w, http.StatusForbidden, codeForbidden, "the session is not bound to this device")
		return
	}
	status, data, err := p.Raw(ctx, http.MethodDelete, sessionPath(sid), nil)
	if err != nil {
		apiError(w, http.StatusBadGateway, codeBackendUnavailable, "the hub could not reach the device plugin")
		return
	}
	writeRaw(w, status, data)
}
