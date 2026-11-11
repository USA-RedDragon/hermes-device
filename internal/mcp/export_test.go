package mcp

// Active returns the number of requests in flight on the named server.
func (m *Manager) Active(name string) int {
	s := m.servers[name]
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}
