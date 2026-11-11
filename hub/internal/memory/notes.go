package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/USA-RedDragon/hermes-hub/internal/device"
	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

const maxNotesBody = 4 << 20

const fieldContent = "content"

// notes is a client of the device plugin's memory routes and of the
// dashboard's config route on one backend (hermes-device docs/protocol.md
// section 9.1).
type notes struct {
	r device.Requester
}

type pluginError struct {
	Code    string `json:"error"`
	Message string `json:"message"`
}

func (n *notes) do(ctx context.Context, method, path string, body any) (hub.Notes, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return hub.Notes{}, err
		}
	}
	resp, err := n.r.Request(ctx, method, path, b)
	if err != nil {
		return hub.Notes{}, &hub.MemoryError{Status: http.StatusBadGateway, Message: "Could not reach Hermes"}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxNotesBody))
	if err != nil {
		return hub.Notes{}, &hub.MemoryError{Status: http.StatusBadGateway, Message: "Hermes's answer was cut off"}
	}
	if resp.StatusCode != http.StatusOK {
		return hub.Notes{}, pluginFailure(resp.StatusCode, data)
	}
	var out hub.Notes
	if err := json.Unmarshal(data, &out); err != nil || out.Targets == nil {
		return hub.Notes{}, &hub.MemoryError{Status: http.StatusBadGateway, Message: "Hermes's answer was not understood"}
	}
	for _, t := range []string{hub.TargetMemory, hub.TargetUser} {
		st := out.Targets[t]
		if st.Entries == nil {
			st.Entries = []string{}
		}
		out.Targets[t] = st
	}
	if out.Pending == nil {
		out.Pending = []hub.PendingNote{}
	}
	return out, nil
}

// pluginFailure maps a refusal of the plugin. A 404 without the plugin's
// own error code means the routes are missing: an older plugin, or none.
func pluginFailure(status int, data []byte) error {
	var pe pluginError
	_ = json.Unmarshal(data, &pe)
	if status == http.StatusNotFound && pe.Code == "" {
		return hub.ErrNoMemory
	}
	msg := pe.Message
	if msg == "" {
		msg = fmt.Sprintf("Hermes refused the request (status %d)", status)
	}
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
	default:
		status = http.StatusBadGateway
	}
	return &hub.MemoryError{Status: status, Message: msg}
}

func (n *notes) get(ctx context.Context) (hub.Notes, error) {
	return n.do(ctx, http.MethodGet, device.PluginBase+"/memory", nil)
}

func (n *notes) change(ctx context.Context, target, op, entry, content string, base []string) (hub.Notes, error) {
	var body map[string]any
	switch op {
	case hub.NoteAdd:
		body = map[string]any{fieldContent: content}
	case hub.NoteEdit:
		body = map[string]any{"entry": entry, fieldContent: content}
	case hub.NoteRemove:
		body = map[string]any{"entry": entry}
	case hub.NoteReplace:
		if base == nil {
			base = []string{}
		}
		body = map[string]any{"text": content, "base": base}
	default:
		return hub.Notes{}, &hub.MemoryError{Status: http.StatusNotFound, Message: "unknown change"}
	}
	return n.do(ctx, http.MethodPost, device.PluginBase+"/memory/"+url.PathEscape(target)+"/"+op, body)
}

func (n *notes) resolve(ctx context.Context, id string, approve bool) (hub.Notes, error) {
	verb := "reject"
	if approve {
		verb = "approve"
	}
	return n.do(ctx, http.MethodPost, device.PluginBase+"/memory/pending/"+url.PathEscape(id)+"/"+verb, nil)
}

// setApproval sets memory.write_approval through the dashboard's config
// route, which merges the change into the profile's config.yaml.
func (n *notes) setApproval(ctx context.Context, on bool) error {
	b, err := json.Marshal(map[string]any{"config": map[string]any{"memory": map[string]any{"write_approval": on}}})
	if err != nil {
		return err
	}
	resp, err := n.r.Request(ctx, http.MethodPut, "/api/config", b)
	if err != nil {
		return &hub.MemoryError{Status: http.StatusBadGateway, Message: "Could not reach Hermes"}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxNotesBody))
	if resp.StatusCode != http.StatusOK {
		return &hub.MemoryError{Status: http.StatusBadGateway, Message: fmt.Sprintf("Hermes did not save the setting (status %d)", resp.StatusCode)}
	}
	return nil
}
