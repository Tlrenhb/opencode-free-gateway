package pool

// SetEnabled toggles a pool entry's enabled flag.
func (m *Manager) SetEnabled(id string, enabled bool) {
	if it, ok := m.items[id]; ok {
		it.Enabled = enabled
		m.items[id] = it
	}
}

// SetSource updates the provenance label (manual/txt) for an entry.
func (m *Manager) SetSource(id string, src string) {
	if it, ok := m.items[id]; ok {
		it.Source = src
		m.items[id] = it
	}
}
