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
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
			// Imported http/socks5 proxies are used immediately (matches the
			// original TS gateway: protocol implies usable). Failed requests
			// naturally mark the worker cooldown and rotate — no pre-probe gate.
			Usable: true,
			Source: "txt",
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

// Probe tests whether the proxy can actually carry an HTTPS request to the
// upstream, not just whether its TCP port accepts a connection. A proxy that
// accepts TCP but fails to forward HTTP is marked unusable. Returns latency
// and success.
func (m *Manager) Probe(id string, timeout time.Duration) (latency time.Duration, ok bool) {
	it, found := m.items[id]
	if !found {
		return 0, false
	}
	// 1) fast TCP gate
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(it.Host, strconv.Itoa(it.Port)), timeout)
	if err != nil {
		m.SetUsable(id, false)
		return 0, false
	}
	conn.Close()
	// 2) real HTTP CONNECT / forward test to the upstream
	start := time.Now()
	if !m.probeHTTP(it, timeout) {
		m.SetUsable(id, false)
		return 0, false
	}
	latency = time.Since(start)
	m.SetUsable(id, true)
	return latency, true
}

// ProbeURL is the endpoint used for real-HTTP proxy probes. Set at startup
// from the configured base URL; defaults to the OpenCode Zen models endpoint.
var ProbeURL = "https://opencode.ai/zen/v1/models"

// probeHTTP performs an actual request through the proxy. Any HTTP response
// (even 4xx/5xx) proves the proxy can forward; a transport error means dead.
func (m *Manager) probeHTTP(it item, timeout time.Duration) bool {
	tr := probeTransport(it, timeout)
	if tr == nil {
		return false
	}
	client := &http.Client{Transport: tr, Timeout: timeout + 2*time.Second}
	req, err := http.NewRequest("GET", ProbeURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer public")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return true
}

func probeTransport(it item, timeout time.Duration) http.RoundTripper {
	switch it.Type {
	case "http", "https":
		u := &url.URL{Scheme: "http", Host: net.JoinHostPort(it.Host, strconv.Itoa(it.Port))}
		if it.Username != "" {
			if it.Password != "" {
				u.User = url.UserPassword(it.Username, it.Password)
			} else {
				u.User = url.User(it.Username)
			}
		}
		return &http.Transport{
			Proxy:                 http.ProxyURL(u),
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
			DialContext:           (&net.Dialer{Timeout: timeout}).DialContext,
		}
	case "socks5":
		proxyAddr := net.JoinHostPort(it.Host, strconv.Itoa(it.Port))
		tr := &http.Transport{
			TLSHandshakeTimeout:   timeout,
			ResponseHeaderTimeout: timeout,
		}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialSOCKS5Probe(ctx, proxyAddr, addr, it.Username, it.Password, timeout)
		}
		return tr
	default:
		return nil
	}
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

// dialSOCKS5Probe performs a minimal RFC 1928 SOCKS5 handshake + CONNECT to
// the probe host. If the handshake or CONNECT fails, the proxy is unusable.
func dialSOCKS5Probe(ctx context.Context, proxyAddr, target string, username, password string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) {
		conn.Close()
		return nil, err
	}
	hasAuth := username != ""
	methods := []byte{0x00}
	if hasAuth {
		methods = []byte{0x02}
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return fail(err)
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fail(err)
	}
	if buf[0] != 0x05 {
		return fail(fmt.Errorf("socks5: bad version %d", buf[0]))
	}
	switch buf[1] {
	case 0x00:
	case 0x02:
		if !hasAuth {
			return fail(errors.New("socks5: server requires auth"))
		}
		if len(username) > 255 || len(password) > 255 {
			return fail(errors.New("socks5: creds too long"))
		}
		msg := []byte{0x01, byte(len(username))}
		msg = append(msg, username...)
		msg = append(msg, byte(len(password)))
		msg = append(msg, password...)
		if _, err := conn.Write(msg); err != nil {
			return fail(err)
		}
		if _, err := io.ReadFull(conn, buf); err != nil {
			return fail(err)
		}
		if buf[1] != 0x00 {
			return fail(errors.New("socks5: auth rejected"))
		}
	default:
		return fail(fmt.Errorf("socks5: no acceptable auth (method %d)", buf[1]))
	}
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fail(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fail(errors.New("socks5: bad target port"))
	}
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fail(errors.New("socks5: hostname too long"))
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	pb := make([]byte, 2)
	pb[0] = byte(port >> 8)
	pb[1] = byte(port & 0xff)
	req = append(req, pb...)
	if _, err := conn.Write(req); err != nil {
		return fail(err)
	}
	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fail(err)
	}
	if reply[1] != 0x00 {
		return fail(fmt.Errorf("socks5 connect failed (code %d)", reply[1]))
	}
	return conn, nil
}
