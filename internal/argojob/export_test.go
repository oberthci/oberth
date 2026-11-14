package argojob

// NewIdentityStore creates a store seeded with the given maps.
func NewIdentityStore(release, ci map[string]PerRepoIdentityConfig) *IdentityStore {
	store := &IdentityStore{}
	store.Replace(release, ci)
	return store
}
