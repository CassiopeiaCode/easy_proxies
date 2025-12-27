package state

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Meta describes the identifying information of a node endpoint.
type Meta struct {
	ID     string `json:"id"`
	Host   string `json:"host"`
	Port   uint16 `json:"port"`
	Scheme string `json:"scheme"`
	Name   string `json:"name"`
	URI    string `json:"uri"`
	Source string `json:"source"`
}

// Record is the unified row persisted to disk.
type Record struct {
	Meta           Meta      `json:"meta"`
	Enabled        bool      `json:"enabled"`
	FailureCount   int       `json:"failure_count"`
	LastFailureAt  time.Time `json:"last_failure_at"`
	LastSuccessAt  time.Time `json:"last_success_at"`
	BlacklistUntil time.Time `json:"blacklist_until"`
	LatencyMs      int64     `json:"latency_ms"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Store handles persistence of node states.
type Store struct {
	path           string
	flushInterval  time.Duration
	mu             sync.RWMutex
	data           map[string]*Record
	dirty          bool
	backgroundOnce sync.Once
}

// Open loads (or creates) a state store at the given path.
func Open(path string, flushInterval time.Duration) (*Store, error) {
	if path == "" {
		return nil, errors.New("state store path cannot be empty")
	}
	if flushInterval <= 0 {
		flushInterval = 30 * time.Second
	}
	s := &Store{
		path:          path,
		flushInterval: flushInterval,
		data:          make(map[string]*Record),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var records []Record
	if err := json.Unmarshal(data, &records); err != nil {
		// backwards compatibility: try legacy format keyed by URI
		var legacy []struct {
			URI              string    `json:"uri"`
			Enabled          bool      `json:"enabled"`
			FailureCount     int       `json:"failure_count"`
			BlacklistedUntil time.Time `json:"blacklisted_until"`
			UpdatedAt        time.Time `json:"updated_at"`
		}
		if err2 := json.Unmarshal(data, &legacy); err2 != nil {
			return err
		}
		for _, l := range legacy {
			if l.URI == "" {
				continue
			}
			rec := &Record{
				Meta: Meta{
					ID:  l.URI,
					URI: l.URI,
				},
				Enabled:        l.Enabled,
				FailureCount:   l.FailureCount,
				BlacklistUntil: l.BlacklistedUntil,
				UpdatedAt:      l.UpdatedAt,
			}
			s.data[rec.Meta.ID] = rec
		}
		return nil
	}
	for i := range records {
		rec := records[i]
		if rec.Meta.ID == "" {
			rec.Meta.ID = rec.Meta.URI
		}
		if rec.Meta.ID == "" {
			continue
		}
		// ensure non-nil meta
		recCopy := rec
		s.data[rec.Meta.ID] = &recCopy
	}
	return nil
}

// Start launches a background flush loop bound to ctx.
func (s *Store) Start(ctx context.Context) {
	s.backgroundOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(s.flushInterval)
			// Prune stale records every 6 hours
			pruneTicker := time.NewTicker(6 * time.Hour)
			defer ticker.Stop()
			defer pruneTicker.Stop()
			for {
				select {
				case <-ctx.Done():
					_ = s.Flush()
					return
				case <-ticker.C:
					_ = s.Flush()
				case <-pruneTicker.C:
					// Remove records not updated in 7 days
					s.PruneStale(7 * 24 * time.Hour)
				}
			}
		}()
	})
}

// Snapshot returns all records.
func (s *Store) Snapshot() []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Record, 0, len(s.data))
	for _, rec := range s.data {
		out = append(out, *rec)
	}
	return out
}

// Get returns a record by ID.
func (s *Store) Get(id string) (Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.data[id]
	if !ok {
		return Record{}, false
	}
	return *rec, true
}

// UpdateMeta ensures a record exists and refreshes metadata fields.
func (s *Store) UpdateMeta(meta Meta) {
	if meta.ID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureRecordLocked(meta, false)
}

// Disable marks a node as disabled until the specified time (zero => indefinitely).
func (s *Store) Disable(meta Meta, until time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.ensureRecordLocked(meta, true)
	if rec == nil {
		return
	}
	rec.Enabled = false
	rec.BlacklistUntil = until
	rec.FailureCount++
	rec.LastFailureAt = time.Now()
	rec.UpdatedAt = time.Now()
	s.dirty = true
}

// Enable clears the disabled flag.
func (s *Store) Enable(meta Meta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.ensureRecordLocked(meta, true)
	if rec == nil {
		return
	}
	rec.Enabled = true
	rec.BlacklistUntil = time.Time{}
	rec.FailureCount = 0
	rec.UpdatedAt = time.Now()
	s.dirty = true
}

// RecordFailure increments the failure count but keeps enabled state.
func (s *Store) RecordFailure(meta Meta, blacklistUntil time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.ensureRecordLocked(meta, true)
	if rec == nil {
		return
	}
	rec.FailureCount++
	rec.LastFailureAt = time.Now()
	if !blacklistUntil.IsZero() {
		rec.BlacklistUntil = blacklistUntil
		rec.Enabled = false
	}
	rec.UpdatedAt = time.Now()
	s.dirty = true
}

// RecordSuccess resets failure counters and updates last success timestamp.
func (s *Store) RecordSuccess(meta Meta, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.ensureRecordLocked(meta, true)
	if rec == nil {
		return
	}
	rec.Enabled = true
	rec.FailureCount = 0
	rec.BlacklistUntil = time.Time{}
	rec.LastSuccessAt = time.Now()
	if latency > 0 {
		rec.LatencyMs = latency.Milliseconds()
	}
	rec.UpdatedAt = time.Now()
	s.dirty = true
}

// Remove deletes an ID from the store.
func (s *Store) Remove(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, id)
	s.dirty = true
}

// PruneStale removes records that haven't been updated in the given duration.
// This helps prevent unbounded memory growth from historical node data.
func (s *Store) PruneStale(maxAge time.Duration) int {
	if maxAge <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	pruned := 0
	for id, rec := range s.data {
		if rec.UpdatedAt.Before(cutoff) {
			delete(s.data, id)
			pruned++
		}
	}
	if pruned > 0 {
		s.dirty = true
		log.Printf("[state] pruned %d stale records older than %v", pruned, maxAge)
	}
	return pruned
}

// IsBlocked reports whether the ID should be skipped at the given time.
func (s *Store) IsBlocked(id string, now time.Time) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.data[id]
	if !ok {
		return false
	}
	if rec.Enabled {
		return false
	}
	if !rec.BlacklistUntil.IsZero() && now.After(rec.BlacklistUntil) {
		rec.Enabled = true
		rec.BlacklistUntil = time.Time{}
		rec.UpdatedAt = time.Now()
		s.dirty = true
		return false
	}
	return true
}

// Flush writes current state to disk immediately.
func (s *Store) Flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		log.Printf("[state] flush skipped: no pending changes")
		return nil
	}
	records := make([]Record, 0, len(s.data))
	for _, rec := range s.data {
		records = append(records, *rec)
	}
	s.dirty = false
	s.mu.Unlock()

	log.Printf("[state] flushing %d records to %s", len(records), s.path)

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		log.Printf("[state] flush failed: create dir %s: %v", filepath.Dir(s.path), err)
		return err
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		log.Printf("[state] flush failed: marshal json: %v", err)
		s.markDirty()
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("[state] flush failed: write temp file %s: %v", tmp, err)
		s.markDirty()
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("[state] flush failed: rename temp file %s -> %s: %v", tmp, s.path, err)
		s.markDirty()
		return err
	}
	log.Printf("[state] flush complete: %d records persisted", len(records))
	return nil
}

func (s *Store) markDirty() {
	s.mu.Lock()
	s.dirty = true
	s.mu.Unlock()
}

// Close forces a flush.
func (s *Store) Close() error {
	return s.Flush()
}

func (s *Store) ensureRecordLocked(meta Meta, create bool) *Record {
	if meta.ID == "" {
		return nil
	}
	if rec, ok := s.data[meta.ID]; ok {
		updated := false
		if meta.Host != "" && rec.Meta.Host != meta.Host {
			rec.Meta.Host = meta.Host
			updated = true
		}
		if meta.Port != 0 && rec.Meta.Port != meta.Port {
			rec.Meta.Port = meta.Port
			updated = true
		}
		if meta.Name != "" && rec.Meta.Name != meta.Name {
			rec.Meta.Name = meta.Name
			updated = true
		}
		if meta.URI != "" && rec.Meta.URI != meta.URI {
			rec.Meta.URI = meta.URI
			updated = true
		}
		if meta.Scheme != "" && rec.Meta.Scheme != meta.Scheme {
			rec.Meta.Scheme = meta.Scheme
			updated = true
		}
		if meta.Source != "" && rec.Meta.Source != meta.Source {
			rec.Meta.Source = meta.Source
			updated = true
		}
		if updated {
			rec.UpdatedAt = time.Now()
			s.dirty = true
		}
		return rec
	}
	if !create {
		return nil
	}
	rec := &Record{
		Meta:    meta,
		Enabled: true,
	}
	rec.UpdatedAt = time.Now()
	s.data[meta.ID] = rec
	s.dirty = true
	return rec
}
