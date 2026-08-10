package pool

import "strconv"

// RestoreItem is one persisted pool entry to be restored with its original id.
type RestoreItem struct {
	ID       string
	Name     string
	Type     string
	Host     string
	Port     int
	Username string
	Password string
	Enabled  bool
	Usable   bool
	Source   string
}

// Restore loads persisted entries preserving their ids so worker→proxy
// bindings survive restarts. Duplicate ids are skipped.
func (m *Manager) Restore(items []RestoreItem) (restored int) {
	for _, it := range items {
		if it.ID == "" || it.Host == "" || it.Port <= 0 {
			continue
		}
		if _, dup := m.items[it.ID]; dup {
			continue
		}
		typ := it.Type
		if typ == "" {
			typ = "http"
		}
		name := it.Name
		if name == "" {
			name = typ + "://" + it.Host + ":" + strconv.Itoa(it.Port)
		}
		m.items[it.ID] = item{
			ID:       it.ID,
			Name:     name,
			Type:     typ,
			Host:     it.Host,
			Port:     it.Port,
			Username: it.Username,
			Password: it.Password,
			Enabled:  it.Enabled,
			Usable:   it.Usable,
			Source:   it.Source,
		}
		m.order = append(m.order, it.ID)
		restored++
	}
	return restored
}
