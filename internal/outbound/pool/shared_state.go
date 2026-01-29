package pool

import (
	"sync"
	"sync/atomic"
	"time"

	"easy_proxies/internal/monitor"
)

// sharedMemberState holds failure/blacklist state shared across all pool instances.
// This enables hybrid mode where pool and multi-port modes share the same node state.
type sharedMemberState struct {
	mu       sync.Mutex
	failures int
	entry    atomic.Pointer[monitor.EntryHandle]
	active   atomic.Int32
}

var sharedStateStore sync.Map // map[tag]*sharedMemberState

// acquireSharedState returns the shared state for a tag, creating if needed.
func acquireSharedState(tag string) *sharedMemberState {
	if v, ok := sharedStateStore.Load(tag); ok {
		return v.(*sharedMemberState)
	}
	state := &sharedMemberState{}
	actual, _ := sharedStateStore.LoadOrStore(tag, state)
	return actual.(*sharedMemberState)
}

// lookupSharedState returns the shared state if it exists.
func lookupSharedState(tag string) (*sharedMemberState, bool) {
	v, ok := sharedStateStore.Load(tag)
	if !ok {
		return nil, false
	}
	return v.(*sharedMemberState), true
}

// ResetSharedStateStore clears all shared state (used during config reload).
func ResetSharedStateStore() {
	sharedStateStore.Range(func(key, _ any) bool {
		sharedStateStore.Delete(key)
		return true
	})
}

func (s *sharedMemberState) attachEntry(entry *monitor.EntryHandle) {
	if entry == nil {
		return
	}
	s.entry.Store(entry)
}

func (s *sharedMemberState) entryHandle() *monitor.EntryHandle {
	return s.entry.Load()
}

// recordFailure increments failure count and triggers blacklist if threshold reached.
// Returns: (current failures, blacklisted, blacklist until time)
func (s *sharedMemberState) recordFailure(cause error, _ int, _ time.Duration) (int, bool, time.Time) {
	// Blacklist is disabled; health gating is handled by monitor's 24h success-rate threshold.
	s.mu.Lock()
	s.failures++
	count := s.failures
	s.mu.Unlock()

	if entry := s.entry.Load(); entry != nil {
		entry.RecordFailure(cause)
	}
	return count, false, time.Time{}
}

func (s *sharedMemberState) recordSuccess() {
	s.mu.Lock()
	s.failures = 0
	s.mu.Unlock()

	if entry := s.entry.Load(); entry != nil {
		entry.RecordSuccess()
	}
}

// isBlacklisted checks if the node is currently blacklisted, auto-clearing if expired.
func (s *sharedMemberState) isBlacklisted(_ time.Time) bool {
	// Blacklist is disabled.
	return false
}

func (s *sharedMemberState) forceRelease() {
	// Blacklist is disabled; keep as a no-op for API compatibility.
	s.mu.Lock()
	s.failures = 0
	s.mu.Unlock()
}

func (s *sharedMemberState) incActive() {
	s.active.Add(1)
	if entry := s.entry.Load(); entry != nil {
		entry.IncActive()
	}
}

func (s *sharedMemberState) decActive() {
	s.active.Add(-1)
	if entry := s.entry.Load(); entry != nil {
		entry.DecActive()
	}
}

func (s *sharedMemberState) activeCount() int32 {
	return s.active.Load()
}

// releaseSharedMember clears blacklist state for a tag (called from release functions).
func releaseSharedMember(tag string) {
	if state, ok := lookupSharedState(tag); ok {
		state.forceRelease()
	}
}
