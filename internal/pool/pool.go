// Package pool manages the shared proxy pool: parsing user-supplied proxy
// lines, batch import with dedupe, health probing, and pruning dead entries.
//
// Supported input formats (one per line):
//
//	http://user:pass@host:port
//	http://host:port
//	socks5://user:pass@host:port
//	socks5://host:port
//	host:port                     (assumed HTTP)
package pool

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidLine is wrapped with the line content when a proxy line cannot be
// parsed.
var ErrInvalidLine = errors.New("invalid proxy line")

// Parsed is the result of parsing one proxy line.
type Parsed struct {
	Type     string // http | socks5
	Host     string
	Port     int
	Username string
	Password string
	Raw      string
}

// Key returns the dedupe key (host:port as written).
func (p Parsed) Key() string { return p.Host + ":" + strconv.Itoa(p.Port) }

// ParseLine parses a single proxy line into a Parsed value.
func ParseLine(line string) (Parsed, error) {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return Parsed{}, fmt.Errorf("%w: empty line", ErrInvalidLine)
	}
	s := raw
	if !strings.Contains(s, "://") {
		s = "http://" + s // bare host:port defaults to http
	}
	u, err := url.Parse(s)
	if err != nil {
		return Parsed{}, fmt.Errorf("%w: %q", ErrInvalidLine, raw)
	}
	proto := strings.ToLower(u.Scheme)
	switch proto {
	case "http", "https", "socks5":
		// https proxies are dialed with TLS but presented as http; keep type
		// normalized to socks5 / http for the dialer.
		if proto == "https" {
			proto = "http"
		}
	default:
		return Parsed{}, fmt.Errorf("%w: unsupported scheme %q", ErrInvalidLine, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return Parsed{}, fmt.Errorf("%w: missing host in %q", ErrInvalidLine, raw)
	}
	portStr := u.Port()
	if portStr == "" {
		return Parsed{}, fmt.Errorf("%w: missing port in %q", ErrInvalidLine, raw)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return Parsed{}, fmt.Errorf("%w: bad port in %q", ErrInvalidLine, raw)
	}
	p := Parsed{
		Type: proto,
		Host: host,
		Port: port,
		Raw:  raw,
	}
	if u.User != nil {
		p.Username = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			p.Password = pw
		}
	}
	return p, nil
}

// ParseBatch parses many lines, returning valid entries and the invalid ones.
func ParseBatch(text string) (valid []Parsed, invalid []string) {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		p, err := ParseLine(line)
		if err != nil {
			invalid = append(invalid, truncate(line, 80))
			continue
		}
		valid = append(valid, p)
	}
	return valid, invalid
}

// Manager owns the pool entries used by the gateway.
type Manager struct {
	items map[string]item
	order []string
}

type item struct {
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

// NewManager creates an empty pool manager.
func NewManager() *Manager {
	return &Manager{items: make(map[string]item)}
}

// All returns all entries in insertion order.
func (m *Manager) All() []item {
	out := make([]item, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, m.items[id])
	}
	return out
}

// Get returns a single entry by id.
func (m *Manager) Get(id string) (item, bool) {
	it, ok := m.items[id]
	return it, ok
}

// Import adds parsed proxies, skipping duplicates by host:port.
// Returns the number added, skipped, and any stored IDs.
func (m *Manager) Import(parsed []Parsed) (added, skipped int, ids []string) {
	seen := make(map[string]string, len(m.order))
	for _, id := range m.order {
		it := m.items[id]
		seen[it.Host+":"+strconv.Itoa(it.Port)] = id
	}
	for _, p := range parsed {
		key := p.Key()
		if _, dup := seen[key]; dup {
			skipped++
			continue
		}
		id := newID("px")
		it := item{
			ID:       id,
			Name:     p.Type + "://" + key,
			Type:     p.Type,
			Host:     p.Host,
			Port:     p.Port,
			Username: p.Username,
			Password: p.Password,
			Enabled:  true,
			Usable:   false, // proven by probe before use
			Source:   "txt",
		}
		m.items[id] = it
		m.order = append(m.order, id)
		seen[key] = id
		ids = append(ids, id)
		added++
	}
	return added, skipped, ids
}

// Remove deletes one entry and returns whether it existed.
func (m *Manager) Remove(id string) bool {
	if _, ok := m.items[id]; !ok {
		return false
	}
	delete(m.items, id)
	for i, o := range m.order {
		if o == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return true
}

// Prune deletes every entry that is disabled or unusable.
// Returns removed count and the ids that were removed.
func (m *Manager) Prune() (removed int, ids []string) {
	keep := make([]string, 0, len(m.order))
	for _, id := range m.order {
		it := m.items[id]
		if !it.Enabled || !it.Usable {
			delete(m.items, id)
			ids = append(ids, id)
			removed++
			continue
		}
		keep = append(keep, id)
	}
	m.order = keep
	return removed, ids
}

// SetUsable updates the probe result flag for one entry.
func (m *Manager) SetUsable(id string, usable bool) {
	if it, ok := m.items[id]; ok {
		it.Usable = usable
		m.items[id] = it
	}
}

// Probe dials the proxy and measures latency. A nil result means unreachable.
func (m *Manager) Probe(id string, timeout time.Duration) (latency time.Duration, ok bool) {
	it, found := m.items[id]
	if !found {
		return 0, false
	}
	start := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(it.Host, strconv.Itoa(it.Port)), timeout)
	if err != nil {
		m.SetUsable(id, false)
		return 0, false
	}
	conn.Close()
	latency = time.Since(start)
	m.SetUsable(id, true)
	return latency, true
}

// ProbeAll concurrently probes every enabled entry.
func (m *Manager) ProbeAll(timeout time.Duration) map[string]time.Duration {
	ids := make([]string, 0, len(m.order))
	for _, id := range m.order {
		if m.items[id].Enabled {
			ids = append(ids, id)
		}
	}
	type res struct {
		id      string
		latency time.Duration
		ok      bool
	}
	ch := make(chan res, len(ids))
	for _, id := range ids {
		go func(id string) {
			lat, ok := m.Probe(id, timeout)
			ch <- res{id: id, latency: lat, ok: ok}
		}(id)
	}
	out := make(map[string]time.Duration, len(ids))
	for range ids {
		r := <-ch
		if r.ok {
			out[r.id] = r.latency
		}
	}
	return out
}

func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
