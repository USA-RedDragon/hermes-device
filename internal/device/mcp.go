package device

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/USA-RedDragon/hermes-device/internal/mcp"
	"github.com/USA-RedDragon/hermes-device/protocol"
)

// MCP runs the MCP servers of the device config. *mcp.Manager is one.
type MCP interface {
	// Names returns the configured server names, sorted.
	Names() []string
	List() []mcp.Status
	Tools(ctx context.Context, server string) (json.RawMessage, error)
	Call(ctx context.Context, server, tool string, args json.RawMessage) (json.RawMessage, error)
}

func (s *session) cancelMCP(id string) {
	s.bmu.Lock()
	cancel := s.mcpCalls[id]
	s.bmu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// mcpRequest runs one MCP request (docs/protocol.md section 4.9). Like
// browser requests, MCP requests are never kept across a drop: the leg's
// context cancels them. The servers keep running.
func (s *session) mcpRequest(ctx context.Context, data []byte) {
	var req protocol.MCP
	if err := json.Unmarshal(data, &req); err != nil || req.ID == "" {
		slog.Warn("Ignoring malformed MCP request")
		return
	}
	refuse := func(msg string) {
		_ = s.send(ctx, protocol.MCPResult{Type: protocol.TypeMCPResult, ID: req.ID, Code: protocol.MCPRefused,
			Error: fmt.Sprintf("device %s refused: %s", s.name, msg)})
	}
	switch {
	case !s.mcp:
		refuse("this exec leg has no MCP servers")
		return
	case len(data) > protocol.MaxMCPRequest:
		refuse(fmt.Sprintf("the request is over %d bytes", protocol.MaxMCPRequest))
		return
	case req.Op != protocol.MCPOpList && req.Op != protocol.MCPOpTools && req.Op != protocol.MCPOpCall:
		refuse(fmt.Sprintf("unknown MCP operation %q", cut(req.Op, 32)))
		return
	}
	timeout := time.Duration(req.Timeout * float64(time.Second))
	if timeout <= 0 {
		timeout = protocol.MCPDefaultTimeout
	}
	timeout = min(timeout, protocol.MCPMaxTimeout)
	cctx, cancel := context.WithTimeout(ctx, timeout)
	s.bmu.Lock()
	_, dup := s.mcpCalls[req.ID]
	if !dup {
		s.mcpCalls[req.ID] = cancel
	}
	s.bmu.Unlock()
	if dup {
		cancel()
		refuse(fmt.Sprintf("request id %q is already running", cut(req.ID, 64)))
		return
	}
	s.wg.Go(func() {
		defer cancel()
		res, err := s.runMCP(cctx, req)
		s.bmu.Lock()
		delete(s.mcpCalls, req.ID)
		s.bmu.Unlock()
		out := protocol.MCPResult{Type: protocol.TypeMCPResult, ID: req.ID, OK: err == nil, Result: res}
		var me *mcp.Error
		switch {
		case err == nil && len(res) > protocol.MaxMCPResult:
			out = protocol.MCPResult{Type: protocol.TypeMCPResult, ID: req.ID, Code: protocol.MCPTooLarge,
				Error: fmt.Sprintf("the MCP result is %d bytes, over the %d byte limit", len(res), protocol.MaxMCPResult)}
		case err == nil:
		case errors.As(err, &me):
			out.Code, out.Error = me.Code, me.Message
		default:
			out.Code, out.Error = protocol.MCPFailed, err.Error()
		}
		if err != nil {
			slog.Info("MCP request failed", "op", req.Op, "server", cut(req.Server, 64), "tool", cut(req.Tool, 128),
				"code", out.Code, "error", out.Error)
		}
		data, err := encodeMCPResult(out)
		if err != nil {
			data, _ = encodeMCPResult(protocol.MCPResult{Type: protocol.TypeMCPResult, ID: req.ID, Code: protocol.MCPTooLarge,
				Error: err.Error()})
		}
		_ = s.sendRaw(ctx, data)
	})
}

// maxMCPMessage is the largest mcp_result the device writes: the result
// and room for the other members.
const maxMCPMessage = protocol.MaxMCPResult + 64<<10

// encodeMCPResult encodes r without HTML escapes, which would make a result
// up to six times larger, and checks the size of what goes on the leg.
func encodeMCPResult(r protocol.MCPResult) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, fmt.Errorf("the MCP result is not valid JSON: %w", err)
	}
	if buf.Len() > maxMCPMessage {
		return nil, fmt.Errorf("the MCP result is %d bytes as a message, over the %d byte limit", buf.Len(), maxMCPMessage)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func (s *session) runMCP(ctx context.Context, req protocol.MCP) (json.RawMessage, error) {
	m := s.c.opts.MCP
	switch req.Op {
	case protocol.MCPOpList:
		return json.Marshal(map[string][]mcp.Status{"servers": m.List()})
	case protocol.MCPOpTools:
		return m.Tools(ctx, req.Server)
	default:
		slog.Info("MCP tool call", "server", cut(req.Server, 64), "tool", cut(req.Tool, 128))
		return m.Call(ctx, req.Server, req.Tool, req.Arguments)
	}
}
