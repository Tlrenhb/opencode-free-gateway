package relayproxy

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// outbound header policy:
//   - Content-Type / Accept / Authorization are always set by the gateway
//   - a small allow-list of client headers is forwarded (OpenCode identity +
//     agent metadata)
//   - when CLI synthesis is enabled, missing identity headers are filled with
//     configured defaults and request/session UUIDs
//
// Everything else coming from the client is intentionally dropped — the
// upstream should never see arbitrary client headers.

var opencodeHeaderKeys = []string{
	"x-opencode-session",
	"x-opencode-request",
	"x-opencode-project",
	"x-opencode-client",
}

var agentMetadataKeys = []string{
	"x-session-id",
	"x-title",
}

// buildOutboundHeaders constructs the request headers sent to the upstream.
// clientHeaders are the raw headers received from the /v1 caller.
func (c *Client) buildOutboundHeaders(req *http.Request, apiKey string, clientHeaders http.Header) {
	// Base identity.
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	// Forward the allow-listed client headers (case-insensitive lookup).
	for _, name := range append(opencodeHeaderKeys, agentMetadataKeys...) {
		if v := headerValue(clientHeaders, name); v != "" {
			req.Header.Set(name, v)
		}
	}
	// Client UA is forwarded when present.
	if ua := headerValue(clientHeaders, "User-Agent"); ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	// Session affinity synthesis: if the caller did not send an explicit
	// x-opencode-session, derive it from x-session-affinity / x-session-id
	// so successive requests land on the same upstream session.
	if req.Header.Get("x-opencode-session") == "" {
		affinity := headerValue(clientHeaders, "x-session-affinity")
		if affinity == "" {
			affinity = headerValue(clientHeaders, "x-session-id")
		}
		if affinity != "" {
			req.Header.Set("x-opencode-session", affinity)
			if req.Header.Get("x-opencode-request") == "" {
				req.Header.Set("x-opencode-request", newUUID())
			}
		}
	}

	// Optional CLI identity synthesis.
	if c.cfg.SynthesizeCLI {
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", c.cfg.CLIUserAgent)
		}
		if req.Header.Get("x-opencode-client") == "" {
			req.Header.Set("x-opencode-client", c.cfg.CLIClient)
		}
		if req.Header.Get("x-opencode-project") == "" {
			req.Header.Set("x-opencode-project", c.cfg.CLIProject)
		}
		if req.Header.Get("x-opencode-request") == "" {
			req.Header.Set("x-opencode-request", newUUID())
		}
		if req.Header.Get("x-opencode-session") == "" {
			req.Header.Set("x-opencode-session", newUUID())
		}
	}
}

// headerValue returns a header value by case-insensitive name.
func headerValue(h http.Header, name string) string {
	for k, vv := range h {
		if strings.EqualFold(k, name) && len(vv) > 0 {
			return vv[0]
		}
	}
	return ""
}

// newUUID returns a random RFC 4122-style UUID string.
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	s := hex.EncodeToString(b)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
