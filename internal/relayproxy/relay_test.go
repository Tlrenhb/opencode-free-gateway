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
	out, err := stripClientMetadata(in)
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
	out, err := stripClientMetadata(in)
	if err != nil || !bytes.Equal(out, in) {
		t.Fatalf("non-json should pass through: %q %v", out, err)
	}
	// JSON without the field is unchanged bytes.
	in2 := []byte(`{"model":"x"}`)
	out2, err := stripClientMetadata(in2)
	if err != nil || !bytes.Equal(out2, in2) {
		t.Fatalf("clean json should be unchanged: %q %v", out2, err)
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
