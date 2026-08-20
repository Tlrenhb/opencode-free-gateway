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
	"regexp"
	"strings"
	"time"

	"github.com/Tlrenhb/opencode-free-gateway/internal/config"
	"github.com/Tlrenhb/opencode-free-gateway/internal/rotator"
)

// ErrNoWorker is returned when the rotator has no workers configured.
var ErrNoWorker = errors.New("no workers configured")

// MaxWorkerAttempts caps how many different workers a single client request
// may try before the gateway surfaces the upstream error. 429/free-limit,
// 5xx and transport errors rotate to the next worker; client errors (4xx)
// are returned immediately.
const MaxWorkerAttempts = 3

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

// Forward relays one /v1 request upstream, trying up to MaxWorkerAttempts
// different workers. Worker-level failures (429/free-limit, 5xx, transport
// errors) rotate to the next worker; client errors (4xx) return immediately.
// Only after every attempt fails is the last upstream error surfaced.
func (c *Client) Forward(ctx context.Context, method, path string, query url.Values, header http.Header, rawBody []byte) (*Result, error) {
	upstreamURL := c.buildUpstreamURL(path, query)

	// Minimal body fix: drop client_metadata, normalize effort aliases, and
	// replay reasoning_content for thinking models (upstream requirement).
	outBody, err := transformRequestBody(rawBody)
	if err != nil {
		return nil, fmt.Errorf("body fix: %w", err)
	}
	c.logOutbound(method, path, header, outBody)

	tried := make(map[string]bool)
	var lastResult *Result
	var lastErr error

	for attempt := 0; attempt < MaxWorkerAttempts; attempt++ {
		worker := c.rot.PickExcluding(time.Now(), tried)
		if worker == nil {
			// Every worker is hard-banned or unavailable: stop retrying.
			break
		}
		tried[worker.ID] = true

		result, err := c.attemptOne(ctx, worker, method, upstreamURL, query, header, outBody)
		if err != nil {
			lastErr = err
			continue
		}
		lastResult = result

		switch {
		case result.Status >= 200 && result.Status < 400:
			c.rot.MarkSuccess(worker.ID)
			return result, nil
		case result.Status == http.StatusTooManyRequests:
			// handled inside attemptOne (markBan/markCooldown + body kept)
			continue
		case result.Status >= 500:
			c.rot.MarkCooldown(worker.ID, time.Now())
			continue
		default:
			// 4xx client errors: nothing a different worker would fix.
			return result, nil
		}
	}

	if lastResult != nil {
		c.logger.Warn("all worker attempts failed; surfacing last error",
			"attempts", len(tried), "status", lastResult.Status)
		return lastResult, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ErrNoWorker
}

// logOutbound records the outbound request's model, reasoning effort, key
// request headers, and full body so the exact upstream traffic is observable.
func (c *Client) logOutbound(method, path string, header http.Header, body []byte) {
	attrs := []any{"method", method, "path", path}
	if len(bytes.TrimSpace(body)) > 0 {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err == nil {
			var model string
			if m, ok := payload["model"]; ok {
				_ = json.Unmarshal(m, &model)
			}
			var effort string
			if r, ok := payload["reasoning_effort"]; ok {
				_ = json.Unmarshal(r, &effort)
			}
			var thinking string
			if r, ok := payload["thinking"]; ok {
				_ = json.Unmarshal(r, &thinking)
			}
			attrs = append(attrs, "model", model, "reasoning_effort", effort, "thinking", thinking)
		}
		attrs = append(attrs, "body", string(body))
	}
	if header != nil {
		hdrs := map[string]string{}
		for name, vals := range header {
			v := ""
			if len(vals) > 0 {
				v = vals[0]
			}
			if strings.EqualFold(name, "Authorization") && v != "" {
				if len(v) > 8 {
					v = v[:8] + "..."
				}
			}
			hdrs[name] = v
		}
		attrs = append(attrs, "headers", hdrs)
	}
	c.logger.Info("outbound", attrs...)
}

// attemptOne performs a single upstream request on the given worker.
// On 429 it consumes the body to classify free-limit vs generic rate-limit
// and marks the worker accordingly (ban 24h or cooldown); the body is kept
// so the caller can still surface the upstream payload.
func (c *Client) attemptOne(ctx context.Context, worker *rotator.State, method, upstreamURL string, query url.Values, header http.Header, outBody []byte) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, method, upstreamURL, bytes.NewReader(outBody))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	c.buildOutboundHeaders(req, worker.APIKey, header)
	c.logger.Info("outbound-req", "url", upstreamURL, "headers", outboundRealHeaders(req.Header))

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

	c.logger.Info("forward", "worker", worker.ID, "proxy", proxyID, "url", upstreamURL)

	resp, err := cli.Do(req)
	if err != nil {
		c.rot.MarkError(worker.ID, err.Error(), time.Now())
		c.rot.MarkCooldown(worker.ID, time.Now())
		return nil, fmt.Errorf("upstream request failed: %w", err)
	}

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

	return &Result{Status: resp.StatusCode, Header: resp.Header, Body: resp.Body, WorkerID: worker.ID, ProxyID: proxyID}, nil
}

// CopyResponse streams an upstream result to the downstream client
// (SSE-safe: plain io.Copy, chunked transfer preserved).
func CopyResponse(w http.ResponseWriter, r *Result) {
	CopyResponseWithHook(w, r, nil)
}

// CopyResponseWithHook streams like CopyResponse but also feeds every byte
// to hook (if non-nil) so callers can inspect the payload (e.g. extract the
// SSE usage block for token accounting).
func CopyResponseWithHook(w http.ResponseWriter, r *Result, hook func([]byte)) {
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
		if hook != nil {
			buf := make([]byte, 32*1024)
			for {
				n, err := r.Body.Read(buf)
				if n > 0 {
					hook(buf[:n])
					_, werr := w.Write(buf[:n])
					if werr != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
		} else {
			_, _ = io.Copy(w, r.Body)
		}
		r.Body.Close()
	}
}

// Thinking-model detection + reasoning_content replay, ported from the
// original TypeScript gateway (src/relay/body.ts). OpenCode's upstream
// requires assistant turns to carry reasoning_content for thinking models;
// without a placeholder the upstream errors with:
//
//	"The `reasoning_content` in the thinking mode must be passed back to the API."
var thinkingModelPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)deepseek`),
	regexp.MustCompile(`(?i)\bkimi\b`),
	regexp.MustCompile(`(?i)\bk2\b`),
	regexp.MustCompile(`(?i)\bminimax\b`),
	regexp.MustCompile(`(?i)\bmimo\b`),
}

const reasoningPlaceholder = " "

func isThinkingMessageModel(model string) bool {
	for _, re := range thinkingModelPatterns {
		if re.MatchString(model) {
			return true
		}
	}
	return false
}

// injectReasoningContentForThinkingModel adds a placeholder
// reasoning_content to every assistant message that does not already carry a
// non-empty one. Only runs for thinking models.
func injectReasoningContentForThinkingModel(payload map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	msgsRaw, ok := payload["messages"]
	if !ok {
		return payload, false
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(msgsRaw, &msgs); err != nil {
		return payload, false
	}
	modified := false
	for i, m := range msgs {
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if role != "assistant" {
			continue
		}
		var rc string
		hasRC := false
		if raw, ok := m["reasoning_content"]; ok {
			if err := json.Unmarshal(raw, &rc); err == nil && strings.TrimSpace(rc) != "" {
				hasRC = true
			}
		}
		if hasRC {
			continue
		}
		m["reasoning_content"] = json.RawMessage(`" "`)
		msgs[i] = m
		modified = true
	}
	if modified {
		out, err := json.Marshal(msgs)
		if err != nil {
			return payload, false
		}
		payload["messages"] = out
	}
	return payload, modified
}

// normalizeDeveloperRole rewrites OpenAI-style `developer` roles to `system`.
// The upstream Console schema only accepts system/user/assistant/tool/
// latest_reminder and rejects `developer` with a 400 deserialize error.
func normalizeDeveloperRole(payload map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	msgsRaw, ok := payload["messages"]
	if !ok {
		return payload, false
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(msgsRaw, &msgs); err != nil {
		return payload, false
	}
	modified := false
	for i, m := range msgs {
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if role == "developer" {
			m["role"] = json.RawMessage(`"system"`)
			msgs[i] = m
			modified = true
		}
	}
	if modified {
		out, err := json.Marshal(msgs)
		if err != nil {
			return payload, false
		}
		payload["messages"] = out
	}
	return payload, modified
}

// Effort-tier model aliases (ported from TS EFFORT_TIERS): map
// "deepseek-v4-flash-high" → model "deepseek-v4-flash" + reasoning_effort.
var effortTiers = map[string][]string{
	"deepseek-v4-pro":   {"low", "medium", "high", "max"},
	"deepseek-v4-flash": {"high", "max"},
	"glm-5.2":           {"high", "max"},
	"mimo-v2.5":         {"high", "max"},
	"grok-4.5":          {"low", "medium", "high"},
	"hy3":               {"none", "low", "high"},
	"kimi-k3":           {"max"},
	"qwen3.6-plus":      {"high", "max"},
	"qwen3.7-max":       {"high", "max"},
	"qwen3.7-plus":      {"high", "max"},
}

func parseEffortLevel(model string) (baseModel, effort string, ok bool) {
	for base, levels := range effortTiers {
		for _, lv := range levels {
			if model == base+"-"+lv {
				return base, lv, true
			}
		}
	}
	return "", "", false
}

// transformRequestBody applies the minimal OpenCode free-model request body
// fixes (ported from the original TS gateway):
//  1. drop client_metadata (upstream rejects it)
//  2. effort-tier aliases → base model + reasoning_effort
//  3. thinking models: inject reasoning_content placeholder on assistant turns
//
// Everything else passes through untouched. Non-JSON bodies are returned
// unchanged.
func transformRequestBody(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		// Not JSON — pass through untouched.
		return raw, nil
	}
	// 1) strip client_metadata
	delete(payload, "client_metadata")

	// model for effort/thinking decisions
	var model string
	if m, ok := payload["model"]; ok {
		_ = json.Unmarshal(m, &model)
	}

	// 2) effort-tier alias normalization
	if base, effort, ok := parseEffortLevel(model); ok {
		payload["model"], _ = json.Marshal(base)
		if _, exists := payload["reasoning_effort"]; !exists {
			payload["reasoning_effort"], _ = json.Marshal(effort)
		}
		model = base
	}

	// 3) thinking models: assistant messages need reasoning_content
	if isThinkingMessageModel(model) {
		payload, _ = injectReasoningContentForThinkingModel(payload)
	}

	// 4) developer role -> system (upstream Console rejects developer)
	payload, _ = normalizeDeveloperRole(payload)

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
// The usable flag is a real probe result now: a proxy that fails the HTTP
// probe (TCP open but cannot forward) is treated as missing, so the worker
// falls back to direct egress instead of burning attempts on 502s.
// Persisted usable state is refreshed at boot (ProbeAll) and after imports.
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


// outboundRealHeaders returns a compact masked snapshot of the real outbound
// request headers (what actually goes upstream after buildOutboundHeaders).
func outboundRealHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, vv := range h {
		v := ""
		if len(vv) > 0 {
			v = vv[0]
		}
		if strings.EqualFold(k, "Authorization") && len(v) > 8 {
			out[k] = v[:12] + "..."
		} else {
			out[k] = v
		}
	}
	return out
}
