// Package relayproxy implements the /v1 forwarding path.
//
// The gateway forwards to the upstream with a deliberately *constructed*
// request: the body is passed through after a single compatibility fix
// (dropping client_metadata, which the upstream rejects), and the outbound
// headers are built from an allow-list rather than copied verbatim.
// Responses (including SSE streams) are copied back byte-for-byte.
package relayproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Tlrenhb/ocfreelay-go/internal/config"
	"github.com/Tlrenhb/ocfreelay-go/internal/rotator"
)

// ErrNoWorker is returned when the rotator has no workers configured.
var ErrNoWorker = errors.New("no workers configured")

// ProxyDialer opens egress connections through a configured proxy.
type ProxyDialer interface {
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

// New creates a relay client.
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
	Status   int
	Header   http.Header
	Body     io.ReadCloser
	WorkerID string
	ProxyID  string
}

// ReadBody reads the full incoming body (bounded) so the gateway can apply
// the minimal request fix before forwarding. Chat requests must be fully
// buffered once; the upstream then receives a re-serialized payload.
func ReadBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 32<<20))
}

// Forward relays one /v1 request upstream using the next ready worker.
// The request body should already be buffered via ReadBody (unless empty).
func (c *Client) Forward(ctx context.Context, method, path string, query url.Values, header http.Header, rawBody []byte) (*Result, error) {
	worker := c.rot.Pick(time.Now())
	if worker == nil {
		return nil, ErrNoWorker
	}

	upstreamURL := c.buildUpstreamURL(path, query)

	// Minimal body fix: drop client_metadata (upstream rejects it).
	outBody, err := stripClientMetadata(rawBody)
	if err != nil {
		return nil, fmt.Errorf("body fix: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, upstreamURL, bytes.NewReader(outBody))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	c.buildOutboundHeaders(req, worker.APIKey, header)

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

	// 429 handling: read the (small) body to classify the failure.
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
		return &Result{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body, WorkerID: worker.ID, ProxyID: proxyID}, nil
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		c.rot.MarkSuccess(worker.ID)
	}
	if resp.StatusCode >= 500 {
		c.rot.MarkCooldown(worker.ID, time.Now())
	}
	return &Result{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body, WorkerID: worker.ID, ProxyID: proxyID}, nil
}

// CopyResponse streams an upstream result to the downstream client
// (SSE-safe: plain io.Copy, chunked transfer preserved).
func CopyResponse(w http.ResponseWriter, r *Result) {
	h := w.Header()
	for k, vv := range r.Header {
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

// stripClientMetadata removes the client_metadata field from a chat request
// body. If the body is not JSON (or has no such field), it is returned
// unchanged.
func stripClientMetadata(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		// Not JSON — pass through untouched.
		return raw, nil
	}
	if _, ok := payload["client_metadata"]; !ok {
		return raw, nil
	}
	delete(payload, "client_metadata")
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// buildUpstreamURL joins the base URL with the incoming /v1 path.
// When the base URL already carries the API version (e.g. .../zen/v1), the
// received /v1 prefix is stripped.
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
