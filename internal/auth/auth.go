// Package auth implements two credential layers:
//
//  1. Admin auth: the management panel and admin API require a login with the
//     configured administrator password (stored as a salted PBKDF2-SHA256
//     hash — standard library only). A successful login issues a random
//     session token.
//  2. Call-key auth: client calls to /v1/* require a bearer key from the
//     configured allow-list when RequireCallKeyAuth is enabled.
//
// The package uses only the Go standard library (crypto/pbkdf2 is part of
// the stdlib since Go 1.24).
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SessionTTL is how long an admin session stays valid.
const SessionTTL = 12 * time.Hour

const (
	iterations = 210_000
	saltLen    = 16
	keyLen     = 32
)

// Manager validates passwords, call keys and issues sessions.
type Manager struct {
	mu        sync.Mutex
	adminHash string
	callKeys  map[string]string // key -> label
	sessions  map[string]session
}

type session struct {
	expires time.Time
	label   string
}

// New creates a Manager.
func New(adminHash string) *Manager {
	return &Manager{
		adminHash: adminHash,
		callKeys:  make(map[string]string),
		sessions:  make(map[string]session),
	}
}

// SetAdminHash replaces the stored password hash (used after admin changes it).
func (m *Manager) SetAdminHash(h string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.adminHash = h
}

// SetCallKeys replaces the allowed call-key map.
func (m *Manager) SetCallKeys(keys map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callKeys = make(map[string]string, len(keys))
	for k, v := range keys {
		m.callKeys[k] = v
	}
}

// AdminPasswordSet reports whether an admin password has been configured.
func (m *Manager) AdminPasswordSet() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.adminHash != ""
}

// VerifyPassword checks a plaintext password against the stored hash.
func (m *Manager) VerifyPassword(plain string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.adminHash == "" {
		return false
	}
	ok, err := verifyPassword(plain, m.adminHash)
	return err == nil && ok
}

// Login issues a session token after a successful password check.
// Returns an empty token on failure.
func (m *Manager) Login(plain string) string {
	if !m.VerifyPassword(plain) {
		return ""
	}
	tok := randomToken(32)
	m.mu.Lock()
	m.sessions[tok] = session{expires: time.Now().Add(SessionTTL), label: "admin"}
	m.mu.Unlock()
	return tok
}

// ValidSession reports whether a session token is still valid.
func (m *Manager) ValidSession(tok string) bool {
	if tok == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(s.expires) {
		delete(m.sessions, tok)
		return false
	}
	return true
}

// RevokeSession deletes one session token (logout).
func (m *Manager) RevokeSession(tok string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, tok)
}

// CallKeyOK reports whether the bearer key is allowed for /v1 calls.
func (m *Manager) CallKeyOK(key string) bool {
	if key == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.callKeys[key]
	return ok
}

// CallKeyCount returns how many call keys are configured (for status UI).
func (m *Manager) CallKeyCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.callKeys)
}

// BearerFromHeader extracts a bearer token from an Authorization header.
func BearerFromHeader(v string) string {
	if len(v) > 7 && strings.EqualFold(v[:7], "Bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

// HashPassword hashes a plaintext password for storage (PBKDF2-SHA256).
func HashPassword(plain string) (string, error) {
	return deriveKey(plain)
}

// deriveKey returns "pbkdf2-sha256$iter$salt$key" with all values base64.
func deriveKey(plain string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, plain, salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	return "pbkdf2-sha256$" + strconv.Itoa(iterations) + "$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(key), nil
}

func verifyPassword(plain, stored string) (bool, error) {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false, nil
	}
	it, err := strconv.Atoi(parts[1])
	if err != nil || it < 1 {
		return false, nil
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, err
	}
	got, err := pbkdf2.Key(sha256.New, plain, salt, it, len(want))
	if err != nil {
		return false, err
	}
	return subtleConstantTimeEq(got, want), nil
}

// subtleConstantTimeEq avoids leaking timing differences for short inputs.
func subtleConstantTimeEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// randomToken returns n random bytes hex-encoded (2n hex chars).
func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
