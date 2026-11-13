package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

const maxNotesBody = 4 << 20

const fieldContent = "content"

// notes is a client of the device plugin's memory routes and of the
// dashboard's config route on one backend (docs/protocol.md
// section 9.1).
type notes struct {
	r device.Requester
}

type pluginError struct {
	Code    string `json:"error"`
	Message string `json:"message"`
}

// send makes one request of the plugin and decodes its answer into out.
func (n *notes) send(ctx context.Context, method, path string, body, out any) error {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return err
		}
	}
	resp, err := n.r.Request(ctx, method, path, b)
	if err != nil {
		return &hub.MemoryError{Status: http.StatusBadGateway, Message: "Could not reach Hermes"}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxNotesBody))
	if err != nil {
		return &hub.MemoryError{Status: http.StatusBadGateway, Message: "Hermes's answer was cut off"}
	}
	if resp.StatusCode != http.StatusOK {
		return pluginFailure(resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return errNotUnderstood()
	}
	return nil
}

func errNotUnderstood() error {
	return &hub.MemoryError{Status: http.StatusBadGateway, Message: "Hermes's answer was not understood"}
}

func (n *notes) do(ctx context.Context, method, path string, body any) (hub.Notes, error) {
	var out hub.Notes
	if err := n.send(ctx, method, path, body, &out); err != nil {
		return hub.Notes{}, err
	}
	return out, fill(&out)
}

// tagProjects names the project of each staged project memory write. The
// project-memory plugin stages them in Hermes's memory approval queue, so
// they come with the global pending writes. Without the plugin, or when it
// does not answer, the writes stay untagged.
func (n *notes) tagProjects(ctx context.Context, out hub.Notes, err error) (hub.Notes, error) {
	if err == nil {
		n.tag(ctx, &out)
	}
	return out, err
}

func (n *notes) tag(ctx context.Context, out *hub.Notes) {
	if len(out.Pending) == 0 {
		return
	}
	var staged struct {
		Pending []struct {
			ID       string `json:"id"`
			Identity string `json:"identity"`
		} `json:"pending"`
	}
	if err := n.send(ctx, http.MethodGet, projectBase+"/pending", nil, &staged); err != nil {
		return
	}
	byID := make(map[string]string, len(staged.Pending))
	for _, p := range staged.Pending {
		byID[p.ID] = p.Identity
	}
	for i := range out.Pending {
		if id, ok := byID[out.Pending[i].ID]; ok {
			out.Pending[i].Project = id
		}
	}
}

// fill checks that an answer has its stores and gives empty lists for
// missing ones.
func fill(out *hub.Notes) error {
	if out.Targets == nil {
		return errNotUnderstood()
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
	return nil
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

// changeBody is the plugin's request body of a change.
func changeBody(op, entry, content string, base []string) (map[string]any, error) {
	switch op {
	case hub.NoteAdd:
		return map[string]any{fieldContent: content}, nil
	case hub.NoteEdit:
		return map[string]any{"entry": entry, fieldContent: content}, nil
	case hub.NoteRemove:
		return map[string]any{"entry": entry}, nil
	case hub.NoteReplace:
		if base == nil {
			base = []string{}
		}
		return map[string]any{"text": content, "base": base}, nil
	}
	return nil, &hub.MemoryError{Status: http.StatusNotFound, Message: "unknown change"}
}

func (n *notes) change(ctx context.Context, target, op, entry, content string, base []string) (hub.Notes, error) {
	body, err := changeBody(op, entry, content, base)
	if err != nil {
		return hub.Notes{}, err
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

// projectBase is where the Hermes dashboard mounts the project-memory
// plugin's routes.
const projectBase = "/api/plugins/project-memory"

func fillProject(p *hub.Project) {
	if p.Aliases == nil {
		p.Aliases = []string{}
	}
	if p.Folders == nil {
		p.Folders = []hub.SeenFolder{}
	}
}

func (n *notes) projects(ctx context.Context) ([]hub.Project, error) {
	var out struct {
		Projects []hub.Project `json:"projects"`
	}
	if err := n.send(ctx, http.MethodGet, projectBase+"/projects", nil, &out); err != nil {
		return nil, err
	}
	if out.Projects == nil {
		return nil, errNotUnderstood()
	}
	for i := range out.Projects {
		fillProject(&out.Projects[i])
	}
	return out.Projects, nil
}

func (n *notes) project(ctx context.Context, method, path string, body any) (hub.ProjectNotes, error) {
	var out hub.ProjectNotes
	if err := n.send(ctx, method, path, body, &out); err != nil {
		return hub.ProjectNotes{}, err
	}
	if err := fill(&out.Notes); err != nil {
		return hub.ProjectNotes{}, err
	}
	fillProject(&out.Project)
	return out, nil
}

func (n *notes) projectNotes(ctx context.Context, id string) (hub.ProjectNotes, error) {
	return n.project(ctx, http.MethodGet, projectBase+"/projects/"+url.PathEscape(id), nil)
}

func (n *notes) changeProject(ctx context.Context, id, op, entry, content string, base []string) (hub.ProjectNotes, error) {
	body, err := changeBody(op, entry, content, base)
	if err != nil {
		return hub.ProjectNotes{}, err
	}
	return n.project(ctx, http.MethodPost, projectBase+"/projects/"+url.PathEscape(id)+"/memory/"+op, body)
}
