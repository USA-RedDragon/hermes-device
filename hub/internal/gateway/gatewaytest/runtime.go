package gatewaytest

import "slices"

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
