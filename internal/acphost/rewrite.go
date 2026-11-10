package acphost

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
)

// ACP methods acp-host rewrites.
const (
	methodNew    = "session/new"
	methodLoad   = "session/load"
	methodResume = "session/resume"
	methodFork   = "session/fork"
	methodList   = "session/list"
	methodPrompt = "session/prompt"
)

// JSON-RPC error codes acp-host answers with itself.
const (
	codeParse         = -32700
	codeInvalidReq    = -32600
	codeInvalidParams = -32602
	codeInternal      = -32603
)

type object = map[string]json.RawMessage

// rpcError is a JSON-RPC error that acp-host answers the editor with
// instead of forwarding the message.
type rpcError struct {
	id      json.RawMessage
	code    int
	message string
}

func (e *rpcError) Error() string { return e.message }

func (e *rpcError) response() []byte {
	id := e.id
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": e.code, "message": e.message},
	})
	return b
}

// listRequest remembers a session/list request so its response can be
// filtered.
type listRequest struct {
	dir string
}

// rewriter applies the section 6.2 rewrites for one connection.
type rewriter struct {
	placeholder  string
	bindingsFile string
	deviceID     string
	// lists holds the session/list requests in flight, by id key. Only the
	// WebSocket reader adds and only the stdout reader removes, under mu
	// held by the caller.
	lists map[string]listRequest
}

// idKey returns a comparable form of a JSON-RPC id.
func idKey(id json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, id); err != nil {
		return string(id)
	}
	return b.String()
}

func hasID(m object) bool {
	id, ok := m["id"]
	return ok && string(id) != "null"
}

// fromEditor turns one WebSocket message into one stdin line. It returns a
// *rpcError when the message must be answered instead of forwarded.
func (r *rewriter) fromEditor(data []byte) ([]byte, error) {
	var m object
	if err := json.Unmarshal(data, &m); err != nil {
		if json.Valid(data) {
			return nil, &rpcError{code: codeInvalidReq, message: "hermes-device acp-host: a message must be one JSON-RPC object"}
		}
		return nil, &rpcError{code: codeParse, message: "hermes-device acp-host: parse error"}
	}
	var method string
	if raw, ok := m["method"]; ok {
		if err := json.Unmarshal(raw, &method); err != nil {
			return nil, &rpcError{id: m["id"], code: codeInvalidReq, message: "hermes-device acp-host: method must be a string"}
		}
	}
	switch method {
	case methodNew, methodLoad, methodResume, methodFork:
		if err := r.rewriteParams(m, func(p object) error {
			if method != methodNew {
				if err := r.checkSession(p); err != nil {
					return err
				}
			}
			p["cwd"] = mustJSON(r.placeholder)
			p["mcpServers"] = json.RawMessage("[]")
			delete(p, "mcp_servers")
			return nil
		}); err != nil {
			return nil, err
		}
	case methodList:
		var dir string
		if err := r.rewriteParams(m, func(p object) error {
			raw, ok := p["cwd"]
			delete(p, "cwd")
			if !ok || string(raw) == "null" {
				return nil
			}
			if err := json.Unmarshal(raw, &dir); err != nil || !path.IsAbs(dir) {
				return errors.New("cwd must be an absolute path")
			}
			dir = path.Clean(dir)
			return nil
		}); err != nil {
			return nil, err
		}
		if hasID(m) {
			r.lists[idKey(m["id"])] = listRequest{dir: dir}
		}
	case methodPrompt:
		if err := r.rewriteParams(m, func(p object) error {
			if err := r.checkSession(p); err != nil {
				return err
			}
			return rewritePrompt(p)
		}); err != nil {
			return nil, err
		}
	}
	return json.Marshal(m)
}

// rewriteParams applies fn to m's params object. Params that are missing
// are treated as an empty object; params of another type are refused,
// since they could carry values past the rewrite.
func (r *rewriter) rewriteParams(m object, fn func(object) error) error {
	p := object{}
	if raw, ok := m["params"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return &rpcError{id: m["id"], code: codeInvalidParams, message: "hermes-device acp-host: params must be an object"}
		}
	}
	if err := fn(p); err != nil {
		switch {
		case errors.Is(err, errOtherDevice):
			return &rpcError{id: m["id"], code: codeInvalidParams, message: err.Error()}
		case errors.Is(err, errBindings):
			return &rpcError{id: m["id"], code: codeInternal, message: err.Error()}
		}
		return &rpcError{id: m["id"], code: codeInvalidParams, message: "hermes-device acp-host: " + err.Error()}
	}
	m["params"] = mustJSON(p)
	return nil
}

// errOtherDevice refuses a session bound to a device other than the
// connection's.
var errOtherDevice = errors.New("session is bound to another device")

// checkSession refuses p when its session is bound to another device. It
// folds the field name session_id, which the ACP SDK also accepts, into
// sessionId so the check sees the id Hermes will use.
func (r *rewriter) checkSession(p object) error {
	if alias, ok := p["session_id"]; ok {
		if _, has := p["sessionId"]; !has {
			p["sessionId"] = alias
		}
		delete(p, "session_id")
	}
	raw, ok := p["sessionId"]
	if !ok {
		return nil
	}
	var sid string
	if err := json.Unmarshal(raw, &sid); err != nil {
		return errors.New("sessionId must be a string")
	}
	bindings, err := readBindings(r.bindingsFile)
	if err != nil {
		return errBindings
	}
	if b, ok := bindings[sid]; ok && b.DeviceID != r.deviceID {
		return errOtherDevice
	}
	return nil
}

// errBindings refuses a session call when the bindings file cannot be read,
// so a broken file never lets a call through.
var errBindings = errors.New("hermes-device acp-host: cannot read the bindings file")

func rewritePrompt(p object) error {
	raw, ok := p["prompt"]
	if !ok {
		return nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return errors.New("prompt must be an array")
	}
	for i, b := range blocks {
		var blk object
		if json.Unmarshal(b, &blk) != nil {
			continue
		}
		var typ string
		if json.Unmarshal(blk["type"], &typ) != nil || typ != "resource_link" {
			continue
		}
		var uri string
		_ = json.Unmarshal(blk["uri"], &uri)
		blocks[i] = mustJSON(map[string]string{
			"type": "text",
			"text": fmt.Sprintf("[hermes-device: link %s removed; the bridge inlines device files]", uri),
		})
	}
	p["prompt"] = mustJSON(blocks)
	return nil
}

// fromAgent rewrites one stdout line. Only responses to tracked
// session/list requests change.
func (r *rewriter) fromAgent(line []byte) ([]byte, error) {
	if len(r.lists) == 0 {
		return line, nil
	}
	var m object
	if err := json.Unmarshal(line, &m); err != nil {
		return line, nil
	}
	if _, isReq := m["method"]; isReq || !hasID(m) {
		return line, nil
	}
	key := idKey(m["id"])
	req, ok := r.lists[key]
	if !ok {
		return line, nil
	}
	delete(r.lists, key)
	raw, ok := m["result"]
	if !ok {
		return line, nil
	}
	var res object
	if err := json.Unmarshal(raw, &res); err != nil {
		return line, nil
	}
	var rows []object
	_ = json.Unmarshal(res["sessions"], &rows)
	bindings, err := readBindings(r.bindingsFile)
	kept := []object{}
	for _, row := range rows {
		var sid string
		if json.Unmarshal(row["sessionId"], &sid) != nil {
			continue
		}
		b, ok := bindings[sid]
		if !ok || b.DeviceID != r.deviceID || !within(b.Dir, req.dir) {
			continue
		}
		row["cwd"] = mustJSON(b.Dir)
		kept = append(kept, row)
	}
	res["sessions"] = mustJSON(kept)
	m["result"] = mustJSON(res)
	out, merr := json.Marshal(m)
	if merr != nil {
		return nil, merr
	}
	return out, err
}

// within reports whether dir is base or below it. An empty base matches
// every dir.
func within(dir, base string) bool {
	if base == "" {
		return true
	}
	dir = path.Clean(dir)
	return dir == base || base == "/" || strings.HasPrefix(dir, base+"/")
}

// Binding is one entry of the plugin's bindings file (docs/protocol.md
// section 8.2).
type Binding struct {
	DeviceID string `json:"device_id"`
	Dir      string `json:"dir"`
}

type bindingsFile struct {
	Bindings map[string]Binding `json:"bindings"`
}

// readBindings reads the bindings file. A missing file has no bindings.
// On any other error it returns no bindings and the error, so callers
// fail closed.
func readBindings(file string) (map[string]Binding, error) {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f bindingsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return f.Bindings, nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
