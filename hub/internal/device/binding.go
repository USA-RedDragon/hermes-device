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

// handleBind forwards a bind to the plugin with the token's device id, and
// passes the plugin's answer through.
func (s *Server) handleBind(w http.ResponseWriter, r *http.Request) {
	d := deviceFrom(r)
	var body struct {
		Dir *string `json:"dir"`
	}
	if !decodeBody(r, &body) || body.Dir == nil {
		apiError(w, http.StatusBadRequest, codeBadRequest, `the body must be {"dir": <string>}`)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pluginTimeout)
	defer cancel()
	status, data, err := s.backends[d.BackendID].plugin.Raw(ctx, http.MethodPut, sessionPath(r.PathValue("session")),
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
