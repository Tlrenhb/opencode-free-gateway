// Package stats tracks per-worker request counts, success/failure counts and
// token usage, persisted to worker-stats.json. Deleting a worker never
// removes its historical stats.
package stats

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// TokenUsage mirrors upstream usage payloads (OpenAI / OpenCode formats).
type TokenUsage struct {
	PromptTokens     int64 `json:"promptTokens"`
	CompletionTokens int64 `json:"completionTokens"`
	TotalTokens      int64 `json:"totalTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
}

// WorkerStat is the persisted snapshot for one worker.
type WorkerStat struct {
	AccountID        string  `json:"accountId"`
	RequestCount     int64   `json:"requestCount"`
	ChatCount        int64   `json:"chatCount"`
	ModelsCount      int64   `json:"modelsCount"`
	SuccessCount     int64   `json:"successCount"`
	ErrorCount       int64   `json:"errorCount"`
	PromptTokens     int64   `json:"promptTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	LastRequestAt    *string `json:"lastRequestAt"`
	LastStatus       *int    `json:"lastStatus"`
	// CacheRate computed on the fly (read / prompt) — see Rate().
	rate *float64
}

// Rate returns cache hit rate (0..1) or nil when no prompt tokens.
func (w *WorkerStat) Rate() *float64 {
	if w.PromptTokens <= 0 {
		return nil
	}
	r := float64(w.CacheReadTokens) / float64(w.PromptTokens)
	return &r
}

// Store keeps stats in memory with periodic JSON persistence.
type Store struct {
	mu      sync.Mutex
	path    string
	stats   map[string]*WorkerStat
	persist bool
	dirty   chan struct{}
	close   chan struct{}
}

// New creates a stats store; persist=false keeps it memory-only (tests).
func New(path string, persist bool) *Store {
	s := &Store{
		path:    path,
		stats:   make(map[string]*WorkerStat),
		persist: persist,
		dirty:   make(chan struct{}, 1),
		close:   make(chan struct{}),
	}
	if persist {
		go s.loop()
	}
	return s
}

// Close stops the persistence loop.
func (s *Store) Close() {
	if s.persist {
		close(s.close)
	}
}

// Load reads persisted stats from disk (skipped silently when missing).
func (s *Store) Load() error {
	if !s.persist {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc struct {
		Workers map[string]*WorkerStat `json:"workers"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, w := range doc.Workers {
		if w != nil && id != "" {
			w.AccountID = id
			s.stats[id] = w
		}
	}
	return nil
}

// RecordRequest increments counters for one upstream attempt.
func (s *Store) RecordRequest(account string, kind string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.ensure(account)
	w.RequestCount++
	switch kind {
	case "chat":
		w.ChatCount++
	case "models":
		w.ModelsCount++
	}
	if status >= 200 && status < 400 {
		w.SuccessCount++
	} else {
		w.ErrorCount++
	}
	st := status
	w.LastStatus = &st
	now := time.Now().UTC().Format(time.RFC3339)
	w.LastRequestAt = &now
	s.touch()
}

// AddTokens accumulates usage for a worker.
func (s *Store) AddTokens(account string, u TokenUsage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.ensure(account)
	w.PromptTokens += u.PromptTokens
	w.CompletionTokens += u.CompletionTokens
	w.TotalTokens += u.TotalTokens
	w.CacheReadTokens += u.CacheReadTokens
	w.CacheWriteTokens += u.CacheWriteTokens
	s.touch()
}

// ForAccounts returns snapshots for the given account ids, creating zero
// rows for ids without history.
func (s *Store) ForAccounts(ids []string) []WorkerStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]WorkerStat, 0, len(ids))
	for _, id := range ids {
		if w, ok := s.stats[id]; ok {
			out = append(out, *w)
		} else {
			out = append(out, WorkerStat{AccountID: id})
		}
	}
	return out
}

// Totals sums usage across the given workers.
func (s *Store) Totals(ids []string) TokenUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	var t TokenUsage
	for _, id := range ids {
		if w, ok := s.stats[id]; ok {
			t.PromptTokens += w.PromptTokens
			t.CompletionTokens += w.CompletionTokens
			t.TotalTokens += w.TotalTokens
			t.CacheReadTokens += w.CacheReadTokens
			t.CacheWriteTokens += w.CacheWriteTokens
		}
	}
	return t
}

// ResetAll clears every historical stat (explicit admin action only).
func (s *Store) ResetAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats = make(map[string]*WorkerStat)
	s.touch()
}

// ensure returns (creating if needed) the stat row for an account.
func (s *Store) ensure(id string) *WorkerStat {
	if id == "" {
		id = "unknown"
	}
	w, ok := s.stats[id]
	if !ok {
		w = &WorkerStat{AccountID: id}
		s.stats[id] = w
	}
	return w
}

// touch signals the persistence loop without blocking callers.
func (s *Store) touch() {
	if !s.persist {
		return
	}
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// loop debounce-saves 500ms after changes, then exits on Close.
func (s *Store) loop() {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case <-s.dirty:
			timer.Reset(500 * time.Millisecond)
		case <-timer.C:
			_ = s.persistNow()
			timer.Stop()
		case <-s.close:
			_ = s.persistNow()
			return
		}
	}
}

func (s *Store) persistNow() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	doc := struct {
		Workers map[string]*WorkerStat `json:"workers"`
	}{Workers: s.stats}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
