package rotator

import (
	"testing"
	"time"
)

// TestPickExcluding ensures retry loop skips already-tried workers.
func TestPickExcluding(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec("a", "k1"), spec("b", "k2"), spec("c", "k3")})

	now := time.Now()
	tried := map[string]bool{}

	// 3 attempts must yield 3 distinct workers
	got := map[string]bool{}
	for i := 0; i < 3; i++ {
		w := r.PickExcluding(now, tried)
		if w == nil {
			t.Fatal("nil worker")
		}
		got[w.ID] = true
		tried[w.ID] = true
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 distinct workers, got %v", got)
	}
}

// TestPickExcludingBanSkip: banned workers are skipped, then cursor fallback.
func TestPickExcludingBanSkip(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec("a", "k1"), spec("b", "k2")})

	now := time.Now()
	r.MarkBan("a", BanDuration, now)
	tried := map[string]bool{}

	w := r.PickExcluding(now, tried)
	if w.ID != "b" {
		t.Fatalf("banned 'a' should be skipped, got %q", w.ID)
	}
}
