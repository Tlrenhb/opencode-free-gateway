// Package rotator implements per-worker scheduling with sticky affinity,
// cooldown, hard banning and automatic disable.
//
// Design notes (re-implemented from scratch for the Go rewrite):
//   - Sticky affinity: keep using the same worker until it fails, so the
//     upstream prompt cache stays warm on one account.
//   - A 429 whose body mentions "FreeUsageLimitError" means the whole account
//     hit its free quota: the worker is hard-banned for 24h.
//   - Other failures back off exponentially; too many consecutive failures
//     auto-disable the worker for a short window.
package rotator

import (
	"strings"
	"sync"
	"time"
)

const (
	// BanDuration is how long a free-usage-limit 429 disables a worker.
	BanDuration = 24 * time.Hour
	// AutoDisableAfter is the consecutive failure count that triggers a disable.
	AutoDisableAfter = 5
	// AutoDisableFor is how long the auto-disable lasts.
	AutoDisableFor = 10 * time.Minute

	cooldownBase = 5 * time.Second
	cooldownMax  = 60 * time.Second
)

// State is the runtime state of one worker.
type State struct {
	ID               string
	APIKey           string
	ProxyID          string
	CooldownUntil    time.Time
	BannedUntil      time.Time
	ConsecutiveFails int
	LastError        string
	LastErrorAt      time.Time
}

// Ready reports whether the worker may carry a request now.
func (s *State) Ready(now time.Time) bool {
	return now.After(s.CooldownUntil) && now.After(s.BannedUntil)
}

// Banned reports whether the worker is hard-banned.
func (s *State) Banned(now time.Time) bool { return now.Before(s.BannedUntil) }

// Status is a short machine-readable status label for the admin UI.
func (s *State) Status(now time.Time) string {
	switch {
	case s.Banned(now):
		return "banned-24h"
	case now.Before(s.CooldownUntil):
		return "cooldown"
	default:
		return "ready"
	}
}

// Rotator owns the worker set and the sticky selection cursor.
type Rotator struct {
	mu      sync.Mutex
	workers []*State
	nextIdx int
	now     func() time.Time
}

// New creates an empty rotator (call Sync to populate).
func New() *Rotator {
	return &Rotator{now: time.Now}
}

// Snapshots returns a copy of all worker states (for admin UI / status API).
func (r *Rotator) Snapshots() []State {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]State, 0, len(r.workers))
	for _, w := range r.workers {
		out = append(out, *w)
	}
	return out
}

// Sync replaces the worker set, preserving runtime state (cooldown, bans,
// error history) for workers that keep their id.
func (r *Rotator) Sync(workers []struct {
	ID      string
	APIKey  string
	ProxyID string
}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	prev := make(map[string]*State, len(r.workers))
	for _, w := range r.workers {
		prev[w.ID] = w
	}

	next := make([]*State, 0, len(workers))
	for _, w := range workers {
		state := &State{
			ID:      w.ID,
			APIKey:  w.APIKey,
			ProxyID: w.ProxyID,
		}
		if p, ok := prev[w.ID]; ok {
			state.CooldownUntil = p.CooldownUntil
			state.BannedUntil = p.BannedUntil
			state.ConsecutiveFails = p.ConsecutiveFails
			state.LastError = p.LastError
			state.LastErrorAt = p.LastErrorAt
		}
		next = append(next, state)
	}
	r.workers = next
	if r.nextIdx >= len(r.workers) {
		r.nextIdx = 0
	}
}

// Pick returns the next ready worker with sticky affinity: prefer the current
// cursor worker as long as it is ready; otherwise scan forward.
func (r *Rotator) Pick(now time.Time) *State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pickLocked(now, nil)
}

// PickExcluding returns the next ready worker, skipping any id in `exclude`
// (used by the retry loop so each attempt tries a different worker).
// Falls back to a ready worker even if all are excluded, then to the cursor.
func (r *Rotator) PickExcluding(now time.Time, exclude map[string]bool) *State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pickLocked(now, exclude)
}

// pickLocked is the shared selection logic; caller must hold r.mu.
// Round-robin: every pick advances the cursor so successive requests rotate
// through all ready workers instead of sticking to one (as requested).
func (r *Rotator) pickLocked(now time.Time, exclude map[string]bool) *State {
	if len(r.workers) == 0 {
		return nil
	}
	// 1) scan forward for a ready worker not yet tried, advancing the
	//    cursor each pick so requests round-robin across workers
	for i := 0; i < len(r.workers); i++ {
		idx := (r.nextIdx + i) % len(r.workers)
		w := r.workers[idx]
		if w.Ready(now) && !exclude[w.ID] {
			r.nextIdx = (idx + 1) % len(r.workers)
			return w
		}
	}
	// 2) all ready workers were tried: take any ready one anyway
	for i := 0; i < len(r.workers); i++ {
		w := r.workers[(r.nextIdx+i)%len(r.workers)]
		if w.Ready(now) {
			r.nextIdx = (r.nextIdx + 1) % len(r.workers)
			return w
		}
	}
	// 3) everything in cooldown/banned: prefer a cooldown (recoverable)
	// worker over a hard-banned one; if every worker is hard-banned,
	// return nil so callers can short-circuit instead of hammering
	// accounts that are banned for 24h.
	var cooldown *State
	for _, w := range r.workers {
		if !w.Banned(now) && now.Before(w.CooldownUntil) {
			cooldown = w
			break
		}
	}
	if cooldown != nil {
		r.nextIdx = (r.nextIdx + 1) % len(r.workers)
		return cooldown
	}
	return nil
}

// MarkSuccess resets the consecutive-failure counter.
func (r *Rotator) MarkSuccess(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.workers {
		if w.ID == id {
			w.ConsecutiveFails = 0
			return
		}
	}
}

// MarkCooldown applies exponential backoff and auto-disable logic after a
// non-429 failure (transport error, 5xx, other statuses).
func (r *Rotator) MarkCooldown(id string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.findLocked(id)
	if w == nil {
		return
	}
	w.ConsecutiveFails++
	if w.ConsecutiveFails >= AutoDisableAfter {
		w.CooldownUntil = now.Add(AutoDisableFor)
		w.ConsecutiveFails = 0
		return
	}
	backoff := cooldownBase << (w.ConsecutiveFails - 1)
	if backoff > cooldownMax {
		backoff = cooldownMax
	}
	w.CooldownUntil = now.Add(backoff)
}

// MarkBan hard-bans a worker (typically for free-usage-limit 429)
// for the given duration. Callers pass BanDuration unless overridden.
func (r *Rotator) MarkBan(id string, dur time.Duration, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.findLocked(id)
	if w == nil {
		return
	}
	w.BannedUntil = now.Add(dur)
	w.CooldownUntil = time.Time{}
	w.ConsecutiveFails = 0
}

// MarkError records the last upstream error message for a worker,
// typically called right before MarkCooldown or MarkBan.
func (r *Rotator) MarkError(id string, message string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.findLocked(id)
	if w == nil {
		return
	}
	if len(message) > 400 {
		message = message[:400]
	}
	w.LastError = message
	w.LastErrorAt = now
}

// ReadyCount returns how many workers are currently usable.
func (r *Rotator) ReadyCount(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, w := range r.workers {
		if w.Ready(now) {
			n++
		}
	}
	return n
}

// IsFreeUsageLimit reports whether a 429 response body indicates the
// OpenCode account hit its free-usage limit.
func IsFreeUsageLimit(body string) bool {
	return strings.Contains(body, "FreeUsageLimitError")
}

func (r *Rotator) findLocked(id string) *State {
	for _, w := range r.workers {
		if w.ID == id {
			return w
		}
	}
	return nil
}
