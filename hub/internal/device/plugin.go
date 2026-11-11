package device

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// PluginBase is where the Hermes dashboard mounts the device plugin's
// routes on the gateway listener (docs/protocol.md section 9).
const PluginBase = "/api/plugins/device"

const maxPluginBody = 4 << 20

// Requester sends HTTP requests to a backend's gateway listener with the
// hub's dashboard session. *gateway.Dialer is one.
type Requester interface {
	Request(ctx context.Context, method, path string, body []byte) (*http.Response, error)
}

// Plugin is a client of the device plugin's HTTP routes on one backend.
type Plugin struct {
	r Requester
}

// NewPlugin returns a client that sends its requests through r.
func NewPlugin(r Requester) *Plugin { return &Plugin{r: r} }

// PluginError is an error answer of the plugin, or of the dashboard in
// front of it.
type PluginError struct {
	Status  int
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (e *PluginError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("device plugin: %d %s: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("device plugin: %d %s", e.Status, e.Code)
}

// ErrUnavailable is a plugin that could not be reached or did not answer
// in JSON.
var ErrUnavailable = errors.New("device plugin unavailable")

// Info is the plugin's GET /info answer.
type Info struct {
	PluginVersion   string `json:"plugin_version"`
	HermesVersion   string `json:"hermes_version"`
	ProtocolVersion int    `json:"protocol_version"`
	BackendID       string `json:"backend_id"`
	Placeholder     string `json:"placeholder"`
}

// ConnectedDevice is one device connected to the plugin's exec leg.
type ConnectedDevice struct {
	DeviceID      string   `json:"device_id"`
	Name          string   `json:"name"`
	Hostname      string   `json:"hostname"`
	OS            string   `json:"os"`
	Arch          string   `json:"arch"`
	Home          string   `json:"home"`
	Roots         []string `json:"roots"`
	ClientVersion string   `json:"client_version"`
	ConnectedAt   float64  `json:"connected_at"`
}

// Binding is a session's device binding.
type Binding struct {
	SessionID  string   `json:"session_id"`
	DeviceID   string   `json:"device_id"`
	Dir        string   `json:"dir"`
	Generation string   `json:"generation"`
	BoundAt    float64  `json:"bound_at"`
	Resolved   Resolved `json:"resolved"`
}

// Binding resolution states.
const (
	StateBound      = "bound"
	StateNeverBound = "never_bound"
	StateFailClosed = "fail_closed"
)

// Resolved says which binding serves a session. Sandbox is set only with
// the plugin's sandbox fallback (protocol.md section 8.10): the literal
// null when the sandbox cannot serve the session, else a SandboxBinding.
type Resolved struct {
	State   string          `json:"state"`
	Via     *string         `json:"via"`
	Sandbox json.RawMessage `json:"sandbox,omitempty"`
}

// SandboxBinding is the implicit binding of a never-bound session to the
// sandbox device.
type SandboxBinding struct {
	DeviceID string `json:"device_id"`
	Dir      string `json:"dir"`
}

// Dirs is the plugin's answer to a directory listing on a device.
type Dirs struct {
	Path    string `json:"path"`
	Entries []struct {
		Name string `json:"name"`
	} `json:"entries"`
}

// slowRequester sends a request that only its context bounds.
// *gateway.Dialer is one.
type slowRequester interface {
	RequestSlow(ctx context.Context, method, path string, body []byte) (*http.Response, error)
}

// Raw sends one request and returns the plugin's status and body as they
// are. An error means no answer came.
func (p *Plugin) Raw(ctx context.Context, method, path string, body any) (int, []byte, error) {
	return p.raw(ctx, p.r.Request, method, path, body)
}

func (p *Plugin) raw(ctx context.Context, send func(context.Context, string, string, []byte) (*http.Response, error),
	method, path string, body any) (int, []byte, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return 0, nil, err
		}
	}
	resp, err := send(ctx, method, PluginBase+path, b)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPluginBody))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return resp.StatusCode, data, nil
}

func (p *Plugin) call(ctx context.Context, method, path string, body, out any) error {
	return p.callWith(ctx, p.r.Request, method, path, body, out)
}

func (p *Plugin) callWith(ctx context.Context, send func(context.Context, string, string, []byte) (*http.Response, error),
	method, path string, body, out any) error {
	status, data, err := p.raw(ctx, send, method, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status > 299 {
		e := &PluginError{Status: status}
		_ = json.Unmarshal(data, e)
		if e.Code == "" {
			e.Code = http.StatusText(status)
		}
		return e
	}
	if out == nil || status == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: bad answer to %s %s: %w", ErrUnavailable, method, path, err)
	}
	return nil
}

// Info returns the plugin's versions and placeholder.
func (p *Plugin) Info(ctx context.Context) (Info, error) {
	var out Info
	return out, p.call(ctx, http.MethodGet, "/info", nil, &out)
}

// Devices lists the devices connected to the backend.
func (p *Plugin) Devices(ctx context.Context) ([]ConnectedDevice, error) {
	var out struct {
		Devices []ConnectedDevice `json:"devices"`
	}
	return out.Devices, p.call(ctx, http.MethodGet, "/devices", nil, &out)
}

// Dirs lists the subdirectories of path on a connected device; "" is the
// device's first root.
func (p *Plugin) Dirs(ctx context.Context, deviceID, path string) (Dirs, error) {
	var out Dirs
	q := ""
	if path != "" {
		q = "?path=" + url.QueryEscape(path)
	}
	return out, p.call(ctx, http.MethodGet, "/devices/"+url.PathEscape(deviceID)+"/dirs"+q, nil, &out)
}

// Bindings lists every session binding of the backend.
func (p *Plugin) Bindings(ctx context.Context) ([]Binding, error) {
	var out struct {
		Bindings []Binding `json:"bindings"`
	}
	return out.Bindings, p.call(ctx, http.MethodGet, "/bindings", nil, &out)
}

// Binding returns the binding that serves a session, which may be an
// ancestor's. ok is false for a session no binding serves.
func (p *Plugin) Binding(ctx context.Context, sessionID string) (Binding, bool, error) {
	var out Binding
	err := p.call(ctx, http.MethodGet, sessionPath(sessionID), nil, &out)
	var pe *PluginError
	if errors.As(err, &pe) && pe.Status == http.StatusNotFound && pe.Code == "not_bound" {
		return Binding{}, false, nil
	}
	if err != nil {
		return Binding{}, false, err
	}
	return out, true, nil
}

// Resolve returns how the plugin resolves a session's binding: the
// binding that serves it, which may be an ancestor's, or none.
func (p *Plugin) Resolve(ctx context.Context, sessionID string) (Binding, error) {
	status, data, err := p.Raw(ctx, http.MethodGet, sessionPath(sessionID), nil)
	if err != nil {
		return Binding{}, err
	}
	var out Binding
	if status == http.StatusOK || status == http.StatusNotFound {
		var e PluginError
		_ = json.Unmarshal(data, &e)
		if status == http.StatusNotFound && e.Code != "not_bound" {
			return Binding{}, &PluginError{Status: status, Code: e.Code, Message: e.Message}
		}
		if err := json.Unmarshal(data, &out); err != nil || out.Resolved.State == "" {
			return Binding{}, fmt.Errorf("%w: bad answer to GET %s", ErrUnavailable, sessionPath(sessionID))
		}
		if status == http.StatusNotFound {
			out = Binding{Resolved: out.Resolved}
		}
		return out, nil
	}
	e := &PluginError{Status: status}
	_ = json.Unmarshal(data, e)
	if e.Code == "" {
		e.Code = http.StatusText(status)
	}
	return Binding{}, e
}

// Bind binds a session to a directory of a device.
func (p *Plugin) Bind(ctx context.Context, sessionID, deviceID, dir string) (Binding, error) {
	var out Binding
	return out, p.call(ctx, http.MethodPut, sessionPath(sessionID), map[string]string{keyDeviceID: deviceID, "dir": dir}, &out)
}

// Unbind removes a session's own binding.
func (p *Plugin) Unbind(ctx context.Context, sessionID string) error {
	return p.call(ctx, http.MethodDelete, sessionPath(sessionID), nil, nil)
}

func sessionPath(sessionID string) string {
	return "/sessions/" + url.PathEscape(sessionID) + "/binding"
}

// errorCode is the plugin's error code of err, or a code that says why
// there is none.
func errorCode(err error) string {
	var pe *PluginError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return codeBackendUnavailable
}

// File is the plugin's answer to an upload: where it wrote the file on
// the device.
type File struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	DeviceID string `json:"device_id"`
	Bytes    int64  `json:"bytes"`
}

// PutFile writes a file into the device folder that serves a session
// (docs/protocol.md section 9, upload rules).
func (p *Plugin) PutFile(ctx context.Context, sessionID, name string, data []byte) (File, error) {
	var out File
	body := map[string]string{"name": name, "content_base64": base64.StdEncoding.EncodeToString(data)}
	send := p.r.Request
	if s, ok := p.r.(slowRequester); ok {
		send = s.RequestSlow
	}
	return out, p.callWith(ctx, send, http.MethodPost, "/sessions/"+url.PathEscape(sessionID)+"/files", body, &out)
}
