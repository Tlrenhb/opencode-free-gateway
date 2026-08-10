// Package relayproxy implements egress transports for HTTP and SOCKS5 egress transports for HTTP and SOCKS5
// proxies using only the standard library. HTTP proxies use
// http.Transport.Proxy; SOCKS5 uses a small RFC 1928 dialer wired into
// Transport.DialContext.
package relayproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Tlrenhb/opencode-free-gateway/internal/config"
)

// Dialer caches transports keyed by proxy endpoint so connections are pooled.
type Dialer struct {
	cache map[string]http.RoundTripper
}

// NewDialer creates a Dialer with an internal transport cache.
func NewDialer() *Dialer {
	return &Dialer{cache: make(map[string]http.RoundTripper)}
}

// Close releases cached transport connections.
func (d *Dialer) Close() {
	for _, tr := range d.cache {
		if c, ok := tr.(*http.Transport); ok {
			c.CloseIdleConnections()
		}
	}
	d.cache = map[string]http.RoundTripper{}
}

// TransportFor returns a RoundTripper that egresses via the given proxy.
// A nil proxy returns a direct transport (also cached per "direct" key).
func (d *Dialer) TransportFor(p *config.Proxy) (http.RoundTripper, error) {
	if p == nil || p.IsZero() {
		return d.direct(), nil
	}
	key := proxyCacheKey(*p)
	if tr, ok := d.cache[key]; ok {
		return tr, nil
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           d.socksDialContext(p),
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 0,
	}
	if p.Type == "http" || p.Type == "https" {
		tr.Proxy = http.ProxyURL(proxyURL(*p))
		tr.DialContext = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	}
	d.cache[key] = tr
	return tr, nil
}

func (d *Dialer) direct() http.RoundTripper {
	key := "direct"
	if tr, ok := d.cache[key]; ok {
		return tr
	}
	tr := &http.Transport{
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        50,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	tr.DialContext = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	d.cache[key] = tr
	return tr
}

func proxyCacheKey(p config.Proxy) string {
	return p.Type + "://" + p.Host + ":" + strconv.Itoa(p.Port)
}

// proxyURL builds a *url.URL usable by http.Transport.Proxy (HTTP proxies).
func proxyURL(p config.Proxy) *url.URL {
	u := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
	}
	if p.Username != "" {
		if p.Password != "" {
			u.User = url.UserPassword(p.Username, p.Password)
		} else {
			u.User = url.User(p.Username)
		}
	}
	return u
}

// socksDialContext returns a dialer that connects through a SOCKS5 proxy.
func (d *Dialer) socksDialContext(p *config.Proxy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	proxyAddr := net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialSOCKS5(ctx, proxyAddr, addr, p.Username, p.Password)
		if err != nil {
			return nil, err
		}
		return conn, nil
	}
}

// dialSOCKS5 implements the SOCKS5 handshake (RFC 1928) over a plain TCP
// connection, with optional username/password auth (RFC 1929).
func dialSOCKS5(ctx context.Context, proxyAddr, target string, username, password string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("socks5 dial %s: %w", proxyAddr, err)
	}
	fail := func(err error) (net.Conn, error) {
		conn.Close()
		return nil, err
	}

	hasAuth := username != ""
	// greeting: version, nmethods, methods (0x00 no-auth, 0x02 user/pass)
	methods := []byte{0x00}
	if hasAuth {
		methods = []byte{0x02}
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return fail(fmt.Errorf("socks5 greeting: %w", err))
	}

	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fail(fmt.Errorf("socks5 greeting reply: %w", err))
	}
	if buf[0] != 0x05 {
		return fail(errors.New("socks5: bad version in greeting reply"))
	}

	switch buf[1] {
	case 0x00: // no auth
	case 0x02: // user/pass
		if !hasAuth {
			return fail(errors.New("socks5: server requires auth but none given"))
		}
		if err := socksAuth(conn, username, password); err != nil {
			return fail(err)
		}
	case 0xff:
		return fail(errors.New("socks5: no acceptable auth method"))
	default:
		return fail(fmt.Errorf("socks5: unexpected method 0x%02x", buf[1]))
	}

	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return fail(fmt.Errorf("socks5 target: %w", err))
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return fail(errors.New("socks5: invalid target port"))
	}

	req := []byte{0x05, 0x01, 0x00} // connect
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
		req = append(req, []byte(host)...)
	}
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(port))
	req = append(req, portBytes...)

	if _, err := conn.Write(req); err != nil {
		return fail(fmt.Errorf("socks5 connect: %w", err))
	}

	reply := make([]byte, 4)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fail(fmt.Errorf("socks5 connect reply: %w", err))
	}
	if reply[1] != 0x00 {
		return fail(fmt.Errorf("socks5 connect failed (code %d)", reply[1]))
	}
	// skip the bound address (varies by ATYP)
	switch reply[3] {
	case 0x01:
		_, _ = io.CopyN(io.Discard, conn, 6)
	case 0x04:
		_, _ = io.CopyN(io.Discard, conn, 18)
	case 0x03:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return fail(fmt.Errorf("socks5 bound addr: %w", err))
		}
		_, _ = io.CopyN(io.Discard, conn, int64(l[0])+2)
	default:
		return fail(errors.New("socks5: bad ATYP in reply"))
	}
	return conn, nil
}

// socksAuth performs the RFC 1929 username/password sub-negotiation.
func socksAuth(conn net.Conn, username, password string) error {
	if len(username) > 255 || len(password) > 255 {
		return errors.New("socks5: credentials too long")
	}
	msg := []byte{0x01, byte(len(username))}
	msg = append(msg, []byte(username)...)
	msg = append(msg, byte(len(password)))
	msg = append(msg, []byte(password)...)
	if _, err := conn.Write(msg); err != nil {
		return fmt.Errorf("socks5 auth: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks5 auth reply: %w", err)
	}
	if reply[1] != 0x00 {
		return errors.New("socks5: auth rejected")
	}
	return nil
}
