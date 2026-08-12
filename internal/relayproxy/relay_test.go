package relayproxy

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Tlrenhb/opencode-free-gateway/internal/config"
)

func TestStripClientMetadata(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4-flash-free","client_metadata":{"foo":"bar"},"messages":[]}`)
	out, err := transformRequestBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["client_metadata"]; ok {
		t.Fatal("client_metadata still present")
	}
	if _, ok := m["model"]; !ok {
		t.Fatal("model lost")
	}
	if _, ok := m["messages"]; !ok {
		t.Fatal("messages lost")
	}
}

func TestStripClientMetadataPassthrough(t *testing.T) {
	// Non-JSON body must be untouched.
	in := []byte("not json at all")
	out, err := transformRequestBody(in)
	if err != nil || !bytes.Equal(out, in) {
		t.Fatalf("non-json should pass through: %q %v", out, err)
	}
	// JSON without the field is semantically unchanged (byte order may vary
	// after re-marshal).
	in2 := []byte(`{"model":"x"}`)
	out2, err := transformRequestBody(in2)
	if err != nil {
		t.Fatalf("clean json error: %v", err)
	}
	var a, b map[string]any
	_ = json.Unmarshal(in2, &a)
	_ = json.Unmarshal(out2, &b)
	if len(a) != len(b) || b["model"] != "x" {
		t.Fatalf("clean json should be unchanged semantically: %s -> %s", in2, out2)
	}
}

// TestThinkingReasoningContentInjection verifies the ported TS behaviour:
// thinking models (deepseek…) get a placeholder reasoning_content on
// assistant messages lacking one; non-thinking models are untouched.
func TestThinkingReasoningContentInjection(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4-flash-free","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"ok"},{"role":"user","content":"again"}]}`)
	out, err := transformRequestBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	// assistant message should now carry reasoning_content placeholder
	found := false
	for _, msg := range m.Messages {
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		if role != "assistant" {
			continue
		}
		var rc string
		if err := json.Unmarshal(msg["reasoning_content"], &rc); err != nil {
			t.Fatalf("assistant missing reasoning_content: %s", out)
		}
		if rc != " " {
			t.Fatalf("expected placeholder, got %q", rc)
		}
		found = true
	}
	if !found {
		t.Fatal("no assistant message found")
	}
}

// TestEffortAliasNormalization verifies "deepseek-v4-flash-high" →
// "deepseek-v4-flash" + reasoning_effort=high (ported from TS EFFORT_TIERS).
func TestEffortAliasNormalization(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4-flash-high","messages":[]}`)
	out, err := transformRequestBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	var model, effort string
	_ = json.Unmarshal(m["model"], &model)
	_ = json.Unmarshal(m["reasoning_effort"], &effort)
	if model != "deepseek-v4-flash" || effort != "high" {
		t.Fatalf("expected deepseek-v4-flash/high, got %s/%s", model, effort)
	}
}

func TestBuildOutboundHeaders(t *testing.T) {
	cfg := &config.Settings{
		SynthesizeCLI: true,
		CLIUserAgent:  "opencode-cli/1.0.0",
		CLIClient:     "cli",
		CLIProject:    "default",
	}
	c := &Client{cfg: cfg}

	clientHeaders := httptest.NewRequest("POST", "/v1/chat/completions", nil).Header
	clientHeaders.Set("X-OpenCode-Client", "my-agent")
	clientHeaders.Set("X-Session-Id", "sess-abc")
	clientHeaders.Set("X-OpenCode-Project", "proj-x")
	// a non-allow-listed header must be dropped
	clientHeaders.Set("X-Custom-Evil", "drop-me")
	clientHeaders.Set("Cookie", "secret=1")

	req := httptest.NewRequest("POST", "/upstream", nil)
	c.buildOutboundHeaders(req, "sk-worker", clientHeaders)

	tests := []struct {
		name string
		want string
	}{
		{"Content-Type", "application/json"},
		{"Authorization", "Bearer sk-worker"},
		{"X-OpenCode-Client", "my-agent"},
		{"X-Session-Id", "sess-abc"},
		{"X-OpenCode-Project", "proj-x"},
	}
	for _, tt := range tests {
		if got := req.Header.Get(tt.name); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, got, tt.want)
		}
	}
	// session affinity: x-session-id filled x-opencode-session w/o explicit one
	if got := req.Header.Get("X-OpenCode-Session"); got != "sess-abc" {
		t.Errorf("x-opencode-session = %q, want sess-abc (affinity)", got)
	}
	if req.Header.Get("X-OpenCode-Request") == "" {
		t.Error("x-opencode-request should be synthesized")
	}
	// non-allow-listed headers dropped
	if req.Header.Get("X-Custom-Evil") != "" || req.Header.Get("Cookie") != "" {
		t.Error("non-allow-listed headers leaked upstream")
	}
	// CLI defaults applied (UA + client already present)
	if got := req.Header.Get("User-Agent"); got != "opencode-cli/1.0.0" {
		t.Errorf("UA = %q", got)
	}
}

func TestBuildOutboundHeadersNoSynthesis(t *testing.T) {
	cfg := &config.Settings{SynthesizeCLI: false}
	c := &Client{cfg: cfg}
	clientHeaders := httptest.NewRequest("POST", "/", nil).Header
	req := httptest.NewRequest("POST", "/upstream", nil)
	c.buildOutboundHeaders(req, "", clientHeaders)
	// No caller headers, no synthesis → session/request stay empty.
	if req.Header.Get("X-OpenCode-Session") != "" {
		t.Error("no session should be synthesized when synthesis disabled")
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("no auth without key")
	}
}
