// Package config loads and persists gateway settings.
//
// The on-disk format (data/settings.json) is intentionally kept compatible
// with the legacy TypeScript gateway so existing deployments can migrate
// without touching their data files.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Proxy defines an egress proxy bound to a worker.
type Proxy struct {
	Type     string `json:"type"` // "http" | "socks5"
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// IsZero reports whether the proxy carries no usable endpoint.
func (p Proxy) IsZero() bool {
	return p.Host == "" || p.Port <= 0
}

// Worker is one upstream account (an OpenCode API key).
type Worker struct {
	ID      string `json:"id"`
	APIKey  string `json:"apiKey"`
	ProxyID string `json:"proxyId,omitempty"`
}

// CallKey is a client-facing bearer key allowed to call the gateway.
type CallKey struct {
	Key     string `json:"key"`
	Name    string `json:"name,omitempty"`
	Enabled bool   `json:"enabled"`
}

// PoolProxy is one entry in the shared proxy pool.
type PoolProxy struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"` // http | socks5
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Enabled  bool   `json:"enabled"`
	Usable   bool   `json:"usable"`
	Source   string `json:"source"` // manual | txt
}

// Settings is the full gateway configuration.
type Settings struct {
	BaseURL            string      `json:"baseUrl"`
	ListenPort         int         `json:"port"`
	SynthesizeCLI      bool        `json:"synthesizeCliHeaders"`
	CLIUserAgent       string      `json:"cliUserAgent"`
	CLIClient          string      `json:"cliClient"`
	CLIProject         string      `json:"cliProject"`
	Workers            []Worker    `json:"workers"`
	ProxyPool          []PoolProxy `json:"proxyPool"`
	CallKeys           []CallKey   `json:"callKeys"`
	AdminPasswordHash  string      `json:"adminPasswordHash,omitempty"`
	FreeModelsFilter   bool        `json:"freeModelsFilter"`
	RequireCallKeyAuth bool        `json:"requireCallKeyAuth"`
	// LegacyAccounts maps 1:1 to the old `accounts` array (kept for imports).
	LegacyAccounts []json.RawMessage `json:"accounts,omitempty"`
}

// Default returns a sane zero-config setup.
func Default() *Settings {
	return &Settings{
		BaseURL:            "https://opencode.ai/zen/v1",
		ListenPort:         9876,
		SynthesizeCLI:      false,
		CLIUserAgent:       "opencode-cli/1.0.0",
		CLIClient:          "cli",
		CLIProject:         "default",
		Workers:            []Worker{{ID: "default"}},
		ProxyPool:          []PoolProxy{},
		CallKeys:           []CallKey{},
		FreeModelsFilter:   false,
		RequireCallKeyAuth: false,
	}
}

// Store reads and writes Settings from a JSON file.
type Store struct {
	path string
}

// NewStore creates a Store rooted at the given settings file path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// DefaultDataDir returns the conventional data directory.
func DefaultDataDir() string {
	if d := os.Getenv("OCFREELAY_DATA_DIR"); d != "" {
		return d
	}
	return "data"
}

// SettingsPath returns the settings file location, honoring env override.
func SettingsPath() string {
	if p := os.Getenv("OCFREELAY_SETTINGS_PATH"); p != "" {
		return p
	}
	return filepath.Join(DefaultDataDir(), "settings.json")
}

// Load reads settings from disk; a missing file yields defaults.
func (s *Store) Load() (*Settings, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return nil, fmt.Errorf("read settings: %w", err)
	}
	cfg := Default()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse settings %s: %w", s.path, err)
	}
	cfg.applyEnvOverrides()
	return cfg, nil
}

// Save writes settings atomically (tmp + rename).
func (s *Store) Save(cfg *Settings) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// applyEnvOverrides lets operators override sensitive values without editing
// the settings file. Only applied when the env var is non-empty.
func (c *Settings) applyEnvOverrides() {
	if v := os.Getenv("OCFREELAY_ADMIN_PASSWORD"); v != "" && c.AdminPasswordHash == "" {
		c.AdminPasswordHash = hashPlain(v)
	}
	if v := os.Getenv("OCFREELAY_CALL_KEY"); v != "" {
		c.CallKeys = append(c.CallKeys, CallKey{Key: v, Name: "env", Enabled: true})
	}
}

// hashPlain is a placeholder; the auth package injects the real bcrypt hasher
// via package-level override to keep config free of heavy deps.
var hashPlain = func(s string) string { return "plain:" + s }

// SetHashFunc overrides the password hashing used by env override.
func SetHashFunc(fn func(string) string) { hashPlain = fn }

// FindWorker returns the worker with the given id.
func (c *Settings) FindWorker(id string) (Worker, bool) {
	for _, w := range c.Workers {
		if w.ID == id {
			return w, true
		}
	}
	return Worker{}, false
}

// FindPoolProxy returns a pool entry by id.
func (c *Settings) FindPoolProxy(id string) (PoolProxy, bool) {
	for _, p := range c.ProxyPool {
		if p.ID == id {
			return p, true
		}
	}
	return PoolProxy{}, false
}

// CallKeyAllowed reports whether key is an enabled call key.
func (c *Settings) CallKeyAllowed(key string) bool {
	for _, k := range c.CallKeys {
		if k.Enabled && k.Key != "" && strings.EqualFold(k.Key, key) {
			return true
		}
	}
	return false
}
