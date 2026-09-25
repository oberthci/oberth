package argojob

import "sync/atomic"

// IdentitySnapshot is a frozen, immutable view of the per-repo identity maps.
// Published snapshots are never mutated; Replace installs freshly built maps.
// Old snapshots remain valid as immutable reads -- no reader locking required.
type IdentitySnapshot struct {
	Release map[string]PerRepoIdentityConfig
	CI      map[string]PerRepoIdentityConfig
}

// IdentityStore holds the current per-repo identity configuration behind an
// atomic pointer. Snapshot returns an immutable view that callers may hold
// indefinitely; Replace swaps in a new one without disturbing in-flight reads.
//
// The design is copy-on-write: Replace installs freshly built maps, never
// mutates existing ones. Build reads Config.PerRepoIdentities and
// Config.PerRepoCIIdentities, which are set once from the snapshot at Build
// entry -- no mid-Build swap is possible.
type IdentityStore struct {
	current atomic.Pointer[IdentitySnapshot]
}

// NewIdentityStore creates a store seeded with the given maps.
func NewIdentityStore(release, ci map[string]PerRepoIdentityConfig) *IdentityStore {
	store := &IdentityStore{}
	store.Replace(release, ci)
	return store
}

// Snapshot returns the current immutable view. The returned pointer is safe
// to hold and read indefinitely; a concurrent Replace publishes a new
// snapshot without mutating this one.
func (s *IdentityStore) Snapshot() *IdentitySnapshot {
	return s.current.Load()
}

// Replace installs freshly built identity maps as the new current snapshot.
// Callers must build new maps rather than mutating old ones.
func (s *IdentityStore) Replace(release, ci map[string]PerRepoIdentityConfig) {
	s.current.Store(&IdentitySnapshot{Release: release, CI: ci})
}
