package rotator

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func spec3(id string) struct {
	ID      string
	APIKey  string
	ProxyID string
} {
	return struct {
		ID      string
		APIKey  string
		ProxyID string
	}{ID: id, APIKey: "k-" + id}
}

func mustPick(t *testing.T, r *Rotator, now time.Time) *State {
	t.Helper()
	w := r.Pick(now)
	if w == nil {
		t.Fatal("Pick returned nil, expected a worker")
	}
	return w
}

// --- worker count 0 ---
func TestEdgeZeroWorkers(t *testing.T) {
	r := New()
	now := time.Now()
	if w := r.Pick(now); w != nil {
		t.Fatalf("Pick on empty rotator: expected nil, got %+v", w)
	}
	if w := r.PickExcluding(now, map[string]bool{}); w != nil {
		t.Fatalf("PickExcluding on empty rotator: expected nil, got %+v", w)
	}
	if n := r.ReadyCount(now); n != 0 {
		t.Fatalf("ReadyCount on empty rotator: expected 0, got %d", n)
	}
}

// --- single worker: 3 retries all hit the same worker (sticky + cursor fallback) ---
func TestEdgeSingleWorkerRetriesRepeat(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec3("a")})
	now := time.Now()

	// no cooldown: 3 picks -> same worker
	for i := 0; i < 3; i++ {
		if w := mustPick(t, r, now); w.ID != "a" {
			t.Fatalf("pick %d: expected 'a', got %q", i, w.ID)
		}
	}

	// worker in cooldown: PickExcluding still returns it (cursor fallback)
	r.MarkCooldown("a", now)
	tried := map[string]bool{}
	for i := 0; i < 3; i++ {
		w := r.PickExcluding(now, tried)
		if w == nil {
			t.Fatalf("attempt %d: expected fallback worker, got nil", i)
		}
		if w.ID != "a" {
			t.Fatalf("attempt %d: expected 'a', got %q", i, w.ID)
		}
		tried[w.ID] = true
	}
	// a hard-banned single worker is NOT returned — Pick returns nil so
	// the caller can short-circuit instead of hammering a banned account.
	r.MarkBan("a", BanDuration, now)
	if w := r.Pick(now); w != nil {
		t.Fatalf("single banned worker: expected nil, got %+v", w)
	}
}

// --- Sync preserves ban + cooldown; removed workers drop state; new workers fresh ---
func TestEdgeSyncPreservesBanAndCooldown(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec3("a"), spec3("b")})
	now := time.Now()
	r.MarkBan("a", BanDuration, now)
	r.MarkCooldown("b", now) // 5s backoff

	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec3("a"), spec3("b"), spec3("c")})

	states := map[string]State{}
	for _, s := range r.Snapshots() {
		states[s.ID] = s
	}
	sa, sb, sc := states["a"], states["b"], states["c"]
	if !sa.Banned(now) {
		t.Fatal("ban on 'a' lost across Sync")
	}
	if sb.Status(now) != "cooldown" {
		t.Fatalf("cooldown on 'b' lost across Sync: %q", sb.Status(now))
	}
	if sc.Status(now) != "ready" || sc.ConsecutiveFails != 0 {
		t.Fatal("new worker 'c' should start fresh")
	}
}

// --- banned vs cooldown priority ---
func TestEdgeBanAndCooldownPriority(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec3("a")})
	now := time.Now()

	// cooldown first, then ban: ban wins, and MarkBan clears the cooldown
	r.MarkCooldown("a", now) // until now+5s
	r.MarkBan("a", BanDuration, now)
	st := r.Snapshots()[0]
	if st.Status(now) != "banned-24h" {
		t.Fatalf("expected banned-24h to win over cooldown, got %q", st.Status(now))
	}
	if st.Ready(now) {
		t.Fatal("banned+cooldown worker must not be ready")
	}
	// after ban expiry: cooldown was cleared by MarkBan -> immediately ready
	if !st.Ready(now.Add(BanDuration + time.Second)) {
		t.Fatal("expected ready right after ban expiry (cooldown cleared by ban)")
	}

	// ban first, then a later cooldown (e.g. a failure near ban expiry):
	// both gates apply independently; after ban expiry the cooldown still gates.
	r2 := New()
	r2.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec3("a")})
	r2.MarkBan("a", BanDuration, now)
	late := now.Add(BanDuration) // failure at the very end of the ban
	r2.MarkCooldown("a", late)   // does NOT clear ban; adds 5s cooldown after late
	st2 := r2.Snapshots()[0]
	if st2.Status(now) != "banned-24h" {
		t.Fatalf("expected banned-24h, got %q", st2.Status(now))
	}
	if st2.Ready(now.Add(BanDuration + time.Second)) {
		t.Fatal("cooldown set near ban expiry must still gate readiness after ban expiry")
	}
	if !st2.Ready(now.Add(BanDuration + 6*time.Second)) {
		t.Fatal("worker should be ready after ban AND cooldown expire")
	}
}

// --- all banned: Pick returns nil so callers short-circuit; ReadyCount=0 ---
func TestEdgeAllBannedPick(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec3("a"), spec3("b"), spec3("c")})
	now := time.Now()
	for _, id := range []string{"a", "b", "c"} {
		r.MarkBan(id, BanDuration, now)
	}
	if n := r.ReadyCount(now); n != 0 {
		t.Fatalf("ReadyCount: expected 0, got %d", n)
	}
	// A hard-banned worker must NOT be picked (would hammer a 24h-banned
	// account). nil tells Forward to stop retrying.
	if w := r.Pick(now); w != nil {
		t.Fatalf("expected nil when all workers banned, got %+v", w)
	}
}

// --- exclude all ready workers: falls back to any ready worker ---
func TestEdgeExcludeAllFallback(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec3("a"), spec3("b"), spec3("c")})
	now := time.Now()
	exclude := map[string]bool{"a": true, "b": true, "c": true}
	w := r.PickExcluding(now, exclude)
	if w == nil || !w.Ready(now) {
		t.Fatalf("expected fallback to a ready worker, got %+v", w)
	}
}

// --- MarkSuccess resets the failure counter (backoff restarts at 5s) ---
func TestEdgeMarkSuccessResetsBackoff(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec3("a")})
	now := time.Now()
	for i := 0; i < 4; i++ {
		r.MarkCooldown("a", now.Add(time.Duration(i)*time.Millisecond))
	}
	if got := r.Snapshots()[0].ConsecutiveFails; got != 4 {
		t.Fatalf("expected 4 consecutive fails, got %d", got)
	}
	r.MarkSuccess("a")
	st := r.Snapshots()[0]
	if st.ConsecutiveFails != 0 {
		t.Fatalf("MarkSuccess should reset counter, got %d", st.ConsecutiveFails)
	}
	// next failure must be the base 5s again, not 40s
	r.MarkCooldown("a", now.Add(time.Second))
	if !now.Add(4*time.Second).Before(st.CooldownUntil) || now.Add(6*time.Second).After(st.CooldownUntil) {
		t.Fatalf("expected ~5s backoff after success, got %v", st.CooldownUntil.Sub(now.Add(time.Second)))
	}
}

// --- backoff sequence 5/10/20/40s, 5th fail -> 10min disable + counter reset ---
func TestEdgeBackoffSequence(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec3("a")})
	base := time.Now()
	expect := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second}
	for i, d := range expect {
		now := base.Add(time.Duration(i) * time.Second)
		r.MarkCooldown("a", now)
		got := r.Snapshots()[0].CooldownUntil.Sub(now)
		if got != d {
			t.Fatalf("fail %d: expected backoff %v, got %v", i+1, d, got)
		}
	}
	// 5th fail -> 10-minute auto-disable, counter reset to 0
	now := base.Add(10 * time.Second)
	r.MarkCooldown("a", now)
	st := r.Snapshots()[0]
	if got := st.CooldownUntil.Sub(now); got != AutoDisableFor {
		t.Fatalf("5th fail: expected %v disable, got %v", AutoDisableFor, got)
	}
	if st.ConsecutiveFails != 0 {
		t.Fatalf("counter should reset after auto-disable, got %d", st.ConsecutiveFails)
	}
}

// --- 2 workers, 3 attempts: third attempt repeats the first worker (fallback) ---
func TestEdgeTwoWorkersThreeAttempts(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec3("a"), spec3("b")})
	now := time.Now()
	tried := map[string]bool{}
	seq := []string{}
	for i := 0; i < 3; i++ {
		w := r.PickExcluding(now, tried)
		if w == nil {
			t.Fatal("nil worker")
		}
		seq = append(seq, w.ID)
		tried[w.ID] = true
	}
	if len(seq) != 3 {
		t.Fatalf("expected 3 picks, got %v", seq)
	}
	if seq[0] == seq[1] {
		t.Fatalf("first two picks must differ, got %v", seq)
	}
	// third pick repeats (all excluded -> fallback to ready worker)
	if seq[2] != seq[0] && seq[2] != seq[1] {
		t.Fatalf("third pick should repeat one of the two, got %v", seq)
	}
	t.Logf("2-worker 3-attempt sequence: %v (fallback repeats worker)", seq)
}

// --- concurrency: parallel Pick/Mark on the same rotator must be race-free ---
func TestEdgeConcurrentPickMark(t *testing.T) {
	r := New()
	r.Sync([]struct {
		ID      string
		APIKey  string
		ProxyID string
	}{spec3("a"), spec3("b"), spec3("c")})
	now := time.Now()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				ts := now.Add(time.Duration(i%50) * time.Second)
				w := r.PickExcluding(ts, nil)
				if w == nil {
					// All workers hard-banned at this synthetic time —
					// nil is the intended short-circuit signal.
					continue
				}
				switch (g + i) % 5 {
				case 0:
					r.MarkCooldown(w.ID, ts)
				case 1:
					r.MarkSuccess(w.ID)
				case 2:
					r.MarkBan(w.ID, BanDuration, ts)
				case 3:
					r.MarkError(w.ID, fmt.Sprintf("err %d", i), ts)
				default:
					_ = r.ReadyCount(ts)
					_ = r.Snapshots()
				}
			}
		}(g)
	}
	wg.Wait()
}
