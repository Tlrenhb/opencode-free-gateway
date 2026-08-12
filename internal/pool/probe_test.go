package pool

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProbeDistinguishesForwardingFromTCP verifies the fix for the
// "panel says usable but every request 502s" bug: a proxy whose TCP port
// accepts connections but that cannot forward HTTP must be marked unusable.
func TestProbeDistinguishesForwardingFromTCP(t *testing.T) {
	// upstream: real HTTP server the probe will reach through the proxy
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	probeTarget := upstream.URL

	// 1) working HTTP forward proxy (plain TCP relay to upstream)
	fwdLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer fwdLn.Close()
	go func() {
		for {
			c, err := fwdLn.Accept()
			if err != nil {
				return
			}
			go func(client net.Conn) {
				defer client.Close()
				// read request head (until blank line), then connect upstream
				buf := make([]byte, 4096)
				_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
				n, _ := client.Read(buf)
				_ = client.SetDeadline(time.Time{})
				req := string(buf[:n])
				// parse Host from request line "GET http://host/ HTTP/1.1"
				var host string
				lines := strings.Split(req, "\n")
				if len(lines) > 0 {
					parts := strings.Fields(lines[0])
					if len(parts) > 1 && strings.HasPrefix(parts[1], "http://") {
						rest := parts[1][len("http://"):]
						host = rest[:strings.IndexByte(rest, '/')]
					}
				}
				if host == "" {
					return
				}
				up, err := net.Dial("tcp", host)
				if err != nil {
					return
				}
				defer up.Close()
				_, _ = up.Write([]byte(req))
				// bidirectional copy
				done := make(chan struct{}, 2)
				go func() { _, _ = copyBuf(up, client); done <- struct{}{} }()
				go func() { _, _ = copyBuf(client, up); done <- struct{}{} }()
				<-done
				<-done
			}(c)
		}
	}()

	// 2) dead proxy: TCP accepts, then immediately closes (cannot forward)
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer deadLn.Close()
	go func() {
		for {
			c, err := deadLn.Accept()
			if err != nil {
				return
			}
			c.Close() // accept then drop: TCP "reachable" but useless
		}
	}()

	fwdHost, fwdPort := splitHostPort(t, fwdLn.Addr().String())
	deadHost, deadPort := splitHostPort(t, deadLn.Addr().String())

	oldProbeURL := ProbeURL
	ProbeURL = probeTarget
	defer func() { ProbeURL = oldProbeURL }()

	m := NewManager()
	m.Import([]Parsed{
		{Type: "http", Host: fwdHost, Port: fwdPort},
		{Type: "http", Host: deadHost, Port: deadPort},
	})
	ids := m.All()
	if len(ids) != 2 {
		t.Fatalf("expected 2 proxies, got %d", len(ids))
	}
	// real proxy must be usable
	lat, ok := m.Probe(ids[0].ID, 3*time.Second)
	it0, _ := m.Get(ids[0].ID)
	if !ok {
		t.Fatalf("forwarding proxy marked unusable (lat=%v): %+v", lat, it0)
	}
	// TCP-only proxy must be unusable
	if _, ok := m.Probe(ids[1].ID, 3*time.Second); ok {
		it1, _ := m.Get(ids[1].ID)
		t.Fatalf("dead proxy marked usable: %+v", it1)
	}
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return h, port
}

func copyBuf(dst net.Conn, src net.Conn) (int64, error) {
	buf := make([]byte, 4096)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err != nil {
			return total, err
		}
	}
}
