// Package relayproxy implements the transparent /v1 passthrough.
//
// The gateway treats every /v1/* request as opaque: the path, query, headers
// and body are forwarded to the upstream unchanged. Responses (including SSE
// streams) are streamed back byte-for-byte. The only interception is the
// worker dispatch machinery:
//   - choose a ready worker (sticky affinity, ban/cooldown aware)
//   - route through the worker's bound proxy
//   - on a 429, read the small error body to decide between a 24h ban
//     (FreeUsageLimitError) and a soft cooldown
package relayproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/Tlrenhb/ocfreelay-go/internal/config"
	"github.com/Tlrenhb/ocfreelay-go/internal/rotator"
)

// ErrNoWorker is returned when the rotator has no workers configured.
var ErrNoWorker = errors.New("no workers configured")

// ProxyDialer opens egress connections through a configured proxy.
type ProxyDialer interface {
	// TransportFor returns an http.RoundTripper that egresses via the given
	// proxy (nil proxy = direct). Callers should reuse transports per proxy.
	TransportFor(p *config.Proxy) (http.RoundTripper, error)
}

// Client forwards requests upstream through the worker schedule.
type Client struct {
	cfg     *config.Settings
	rot     *rotator.Rotator
	dialer  ProxyDialer
	logger  *slog.Logger
	httpCli *http.Client
}

// New creates a relay client. transport is used for the direct (no-proxy)
// fallback path; proxied requests build their own transports.
func New(cfg *config.Settings, rot *rotator.Rotator, dialer ProxyDialer, logger *slog.Logger) *Client {
	return &Client{
		cfg:    cfg,
		rot:    rot,
		dialer: dialer,
		logger: logger,
		httpCli: &http.Client{
			Timeout: 0, // streaming: no overall deadline
		},
	}
}

// Result describes one upstream attempt.
type Result struct {
	Status     int
	Header     http.Header
	Body       io.ReadCloser
	WorkerID   string
	ProxyID    string
	Upstream   string
	ContentLen int64
}

// Forward relays one /v1 request upstream using the next ready worker.
// It returns the upstream response for the caller to stream back.
func (c *Client) Forward(ctx context.Context, method, path string, query url.Values, header http.Header, body io.Reader) (*Result, error) {
	worker := c.rot.Pick(time.Now())
	if worker == nil {
		return nil, ErrNoWorker
	}

	upstreamURL := c.buildUpstreamURL(path, query)

	req, err := http.NewRequestWithContext(ctx, method, upstreamURL, body)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	// Copy client headers, then harden identity fields.
	c.prepareHeaders(req, worker.APIKey, header)

	var transport http.RoundTripper
	var proxyID string
	proxy := c.resolveWorkerProxy(worker)
	if proxy != nil {
		tr, err := c.dialer.TransportFor(proxy)
		if err != nil {
			c.rot.MarkError(worker.ID, "proxy transport: "+err.Error(), time.Now())
			c.rot.MarkCooldown(worker.ID, time.Now())
			return nil, fmt.Errorf("build proxy transport: %w", err)
		}
		transport = tr
		proxyID = worker.ProxyID
	}

	cli := c.httpCli
	if transport != nil {
		cli = &http.Client{Transport: transport}
	}

	c.logger.Debug("forward", "worker", worker.ID, "proxy", proxyID, "url", upstreamURL)

	resp, err := cli.Do(req)
	if err != nil {
		c.rot.MarkError(worker.ID, err.Error(), time.Now())
		c.rot.MarkCooldown(worker.ID, time.Now())
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}

	// 429 handling: read the (small) body to classify the failure. This is
	// the one spot we do not stream untouched.
	if resp.StatusCode == http.StatusTooManyRequests {
		bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if readErr != nil {
			bodyBytes = nil
		}
		bodyText := string(bodyBytes)
		c.rot.MarkError(worker.ID, fmt.Sprintf("bad response status code %d, message: %s", resp.StatusCode, truncate(bodyText, 300)), time.Now())
		if rotator.IsFreeUsageLimit(bodyText) {
			c.logger.Info("worker hit free usage limit; banning 24h", "worker", worker.ID)
			c.rot.MarkBan(worker.ID, rotator.BanDuration, time.Now())
		} else {
			c.rot.MarkCooldown(worker.ID, time.Now())
		}
		resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		// keep body so the client still sees the upstream error payload
		return &Result{
			Status:   resp.StatusCode,
			Header:   resp.Header,
			Body:     resp.Body,
			WorkerID: worker.ID,
			ProxyID:  proxyID,
		}, nil
	}

	// Success (or other non-429 status): pass through untouched.
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		c.rot.MarkSuccess(worker.ID)
	}
	if resp.StatusCode >= 500 {
		c.rot.MarkCooldown(worker.ID, time.Now())
	}
	return &Result{
		Status:   resp.StatusCode,
		Header:   resp.Header,
		Body:     resp.Body,
		WorkerID: worker.ID,
		ProxyID:  proxyID,
	}, nil
}

// CopyResponse streams an upstream result to the downstream client
// (SSE-safe: plain io.Copy, chunked transfer preserved).
func CopyResponse(w http.ResponseWriter, r *Result) {
	h := w.Header()
	for k, vv := range r.Header {
		// skip hop-by-hop headers managed by net/http
		if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Keep-Alive") ||
			strings.EqualFold(k, "Transfer-Encoding") || strings.EqualFold(k, "Upgrade") {
			continue
		}
		for _, v := range vv {
			h.Add(k, v)
		}
	}
	w.WriteHeader(r.Status)
	if r.Body != nil {
		_, _ = io.Copy(w, r.Body)
		r.Body.Close()
	}
}

// buildUpstreamURL joins the base URL with the incoming /v1 path.
// When the base URL already carries the API version (e.g. .../zen/v1), the
// received /v1 prefix is stripped to avoid /v1/v1/... duplication.
func (c *Client) buildUpstreamURL(path string, query url.Values) string {
	upstream := strings.TrimRight(c.cfg.BaseURL, "/")
	trimmed := path
	if strings.HasSuffix(upstream, "/v1") {
		trimmed = strings.TrimPrefix(path, "/v1")
	}
	u := upstream + trimmed
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// prepareHeaders copies caller headers and applies identity/stream settings.
func (c *Client) prepareHeaders(req *http.Request, apiKey string, src http.Header) {
	for k, vv := range src {
		lk := strings.ToLower(k)
		// content-length is recomputed by net/http; drop to avoid confusion
		if lk == "content-length" || lk == "host" {
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	if c.cfg.SynthesizeCLI && req.Header.Get("X-OpenCode-Client") == "" {
		req.Header.Set("X-OpenCode-User-Agent", c.cfg.CLIUserAgent)
		req.Header.Set("X-OpenCode-Client", c.cfg.CLIClient)
		req.Header.Set("X-OpenCode-Project", c.cfg.CLIProject)
	}
}

// resolveWorkerProxy finds the worker's bound pool proxy, if any.
func (c *Client) resolveWorkerProxy(w *rotator.State) *config.Proxy {
	if w.ProxyID == "" {
		return nil
	}
	pp, ok := c.cfg.FindPoolProxy(w.ProxyID)
	if !ok || !pp.Enabled || !pp.Usable {
		return nil
	}
	return &config.Proxy{
		Type:     pp.Type,
		Host:     pp.Host,
		Port:     pp.Port,
		Username: pp.Username,
		Password: pp.Password,
	}
}

// IsRetryableError reports whether an upstream I/O error is a transient
// connection fault worth treating as a worker failure (as opposed to a
// client-side cancellation).
func IsRetryableError(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
