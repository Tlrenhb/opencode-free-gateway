package auth

import (
	"testing"
)

func TestHashVerify(t *testing.T) {
	h, err := HashPassword("s3cret!")
	if err != nil {
		t.Fatal(err)
	}
	m := New(h)
	if !m.VerifyPassword("s3cret!") {
		t.Fatal("correct password rejected")
	}
	if m.VerifyPassword("wrong") {
		t.Fatal("wrong password accepted")
	}
}

func TestLoginSession(t *testing.T) {
	h, _ := HashPassword("pass")
	m := New(h)
	if m.Login("bad") != "" {
		t.Fatal("login with bad password should fail")
	}
	tok := m.Login("pass")
	if tok == "" {
		t.Fatal("login should issue token")
	}
	if !m.ValidSession(tok) {
		t.Fatal("session should be valid")
	}
	if m.ValidSession("nope") {
		t.Fatal("random token should be invalid")
	}
	m.RevokeSession(tok)
	if m.ValidSession(tok) {
		t.Fatal("session should be revoked")
	}
}

func TestCallKeys(t *testing.T) {
	m := New("")
	m.SetCallKeys(map[string]string{"sk-1": "a", "sk-2": "b"})
	if !m.CallKeyOK("sk-1") || !m.CallKeyOK("sk-2") {
		t.Fatal("valid keys rejected")
	}
	if m.CallKeyOK("sk-3") {
		t.Fatal("unknown key accepted")
	}
	if m.CallKeyCount() != 2 {
		t.Fatalf("count = %d", m.CallKeyCount())
	}
}

func TestBearer(t *testing.T) {
	if BearerFromHeader("Bearer abc") != "abc" {
		t.Fatal("bearer parse failed")
	}
	if BearerFromHeader("Basic x") != "" {
		t.Fatal("non-bearer should be empty")
	}
}

func TestNoPassword(t *testing.T) {
	m := New("")
	if m.AdminPasswordSet() {
		t.Fatal("empty hash should be unset")
	}
	if m.Login("anything") != "" {
		t.Fatal("login must fail without password")
	}
}
