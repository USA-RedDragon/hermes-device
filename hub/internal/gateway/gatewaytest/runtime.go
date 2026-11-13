package gatewaytest

import (
	"slices"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
)

const methodRuntimeDescribe = "runtime.describe"

// SetCapabilities sets the capabilities runtime.describe reports.
func (g *Gateway) SetCapabilities(caps ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.caps = slices.Clone(caps)
}

func (g *Gateway) hRuntimeDescribe(_ *conn, _ map[string]any) (any, string, func()) {
	return map[string]any{"capabilities": slices.Clone(g.caps)}, "", nil
}

// SetAudience makes session.list with audience keep only the sessions ids;
// an audience that was never set is refused, as when no plugin judges it.
func (g *Gateway) SetAudience(audience string, ids ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.audiences == nil {
		g.audiences = map[string][]string{}
	}
	g.audiences[audience] = slices.Clone(ids)
}

// hBackgroundTool serves session.background_tool as a Hermes that lists
// gateway.CapToolBackground does; without it the method is unknown. A
// running turn answers "requested" with one tool, an idle one "none".
func (g *Gateway) hBackgroundTool(_ *conn, p map[string]any) (any, string, func()) {
	if !slices.Contains(g.caps, gateway.CapToolBackground) {
		return nil, "unknown method: " + gateway.MethodBackgroundTool, nil
	}
	for k := range p {
		if k != kSessionID && k != kExecGen && k != "tool_call_id" {
			return nil, errInvalidParams, nil
		}
	}
	s := g.sessions[str(p, kSessionID)]
	gen, ok := num(p, kExecGen)
	if s == nil || !ok || gen != s.Generation {
		return nil, gateway.ReasonStaleGeneration, nil
	}
	res := gateway.BackgroundToolResult{Status: gateway.BackgroundToolNone}
	if s.Running {
		res = gateway.BackgroundToolResult{Status: gateway.BackgroundToolRequested, Count: 1}
	}
	return res, "", nil
}
