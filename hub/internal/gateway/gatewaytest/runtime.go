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
