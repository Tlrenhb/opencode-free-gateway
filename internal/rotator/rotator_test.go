package rotator

import (
	"testing"
	"time"
)

func spec(id, key string) struct {
	ID      string
	APIKey  string
	ProxyID string
} {
	return struct {
		ID      string
		APIKey  string
		ProxyID string
	}{ID: id, APIKey: key}
}

func TestStickyPick(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec("a", "k1"), spec("b", "k2")})

	now := time.Now()
	first := r.Pick(now)
	if first == nil || first.ID != "a" {
		t.Fatalf("expected first pick 'a', got %+v", first)
	}
	second := r.Pick(now)
	if second.ID != "a" {
		t.Fatalf("expected sticky 'a', got %q", second.ID)
	}
}

func TestBan24h(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec("a", "k1"), spec("b", "k2")})

	now := time.Now()
	r.MarkBan("a", BanDuration, now)

	states := r.Snapshots()
	if states[0].Status(now) != "banned-24h" {
		t.Fatalf("expected banned-24h, got %q", states[0].Status(now))
	}
	// pick must skip banned worker
	p := r.Pick(now)
	if p.ID != "b" {
		t.Fatalf("expected pick 'b', got %q", p.ID)
	}
	// after ban expires the worker is ready again
	if !r.Snapshots()[0].Ready(now.Add(BanDuration + time.Second)) {
		t.Fatal("worker should be ready after ban expiry")
	}
}

func TestAutoDisableAfterConsecutiveFails(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec("a", "k1")})

	now := time.Now()
	for i := 0; i < AutoDisableAfter; i++ {
		r.MarkCooldown("a", now.Add(time.Duration(i)*time.Millisecond))
	}
	st := r.Snapshots()[0]
	if st.ConsecutiveFails != 0 {
		t.Fatalf("consecutiveFails should reset after auto-disable, got %d", st.ConsecutiveFails)
	}
	if !now.Add(AutoDisableFor - time.Second).Before(st.CooldownUntil) {
		t.Fatal("auto-disable window not long enough")
	}
}

func TestMarkErrorRecorded(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec("a", "k1")})

	now := time.Now()
	r.MarkError("a", "bad response status code 429, message: Rate limit exceeded", now)
	st := r.Snapshots()[0]
	if st.LastError == "" || st.LastErrorAt.IsZero() {
		t.Fatal("lastError not recorded")
	}
}

func TestIsFreeUsageLimit(t *testing.T) {
	if !IsFreeUsageLimit(`{"type":"error","error":{"type":"FreeUsageLimitError"}}`) {
		t.Fatal("FreeUsageLimitError not detected")
	}
	if IsFreeUsageLimit(`{"error":"other"}`) {
		t.Fatal("false positive on other error")
	}
}

func TestSyncPreservesState(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec("a", "k1")})
	now := time.Now()
	r.MarkBan("a", BanDuration, now)

	// re-sync with same id: ban must survive
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec("a", "k1new")})
	if r.Snapshots()[0].Status(now) != "banned-24h" {
		t.Fatal("ban state lost across sync")
	}
}
