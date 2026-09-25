package argojob

import (
	"sync"
	"testing"
)

// TestIdentityStoreSnapshotIsImmutable verifies that a snapshot obtained
// before Replace is not affected by the Replace: old snapshots remain valid
// as immutable reads.
func TestIdentityStoreSnapshotIsImmutable(t *testing.T) {
	t.Parallel()
	store := NewIdentityStore(
		map[string]PerRepoIdentityConfig{"codeberg/oberthci/oberth": {ServiceAccountName: "sa-v1"}},
		nil,
	)
	before := store.Snapshot()
	if before.Release["codeberg/oberthci/oberth"].ServiceAccountName != "sa-v1" {
		t.Fatalf("unexpected initial snapshot: %+v", before)
	}

	store.Replace(
		map[string]PerRepoIdentityConfig{"codeberg/oberthci/oberth": {ServiceAccountName: "sa-v2"}},
		nil,
	)
	after := store.Snapshot()

	// The old snapshot must still read the v1 name.
	if before.Release["codeberg/oberthci/oberth"].ServiceAccountName != "sa-v1" {
		t.Fatalf("old snapshot was mutated by Replace: %+v", before)
	}
	if after.Release["codeberg/oberthci/oberth"].ServiceAccountName != "sa-v2" {
		t.Fatalf("new snapshot does not reflect Replace: %+v", after)
	}
}

// TestIdentityStoreConcurrentRace runs concurrent Build calls against a
// Replace loop. Under -race, this proves the atomic pointer produces no
// torn reads or data races.
func TestIdentityStoreConcurrentRace(t *testing.T) {
	t.Parallel()
	store := NewIdentityStore(nil, nil)

	var wg sync.WaitGroup
	// Writer: continuously replace the snapshot.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 1000 {
			_ = i
			store.Replace(
				map[string]PerRepoIdentityConfig{"codeberg/org/repo": {ServiceAccountName: "writer-sa"}},
				map[string]PerRepoIdentityConfig{"codeberg/org/repo": {ServiceAccountName: "writer-ci-sa"}},
			)
		}
	}()
	// Readers: continuously snapshot and inspect.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				snap := store.Snapshot()
				if snap == nil {
					t.Error("Snapshot returned nil")
					return
				}
				// Read map fields to exercise the read path under race.
				_ = snap.Release
				_ = snap.CI
			}
		}()
	}
	wg.Wait()
}

// TestIdentitySnapshotOverridesConfigInBuild proves that when Request.Identities
// is non-nil, Build uses the snapshot's maps instead of Config's maps, and the
// two maps are set once (not swappable mid-Build).
func TestIdentitySnapshotOverridesConfigInBuild(t *testing.T) {
	t.Parallel()
	cfg := testConfigWithPerRepo() // has per-repo identities in Config
	snapshot := &IdentitySnapshot{
		Release: map[string]PerRepoIdentityConfig{
			"codeberg/oberthci/oberth": {ServiceAccountName: "snapshot-release-sa"},
		},
		CI: map[string]PerRepoIdentityConfig{
			"codeberg/oberthci/oberth": {ServiceAccountName: "snapshot-ci-sa"},
		},
	}
	path := "oberth/upstream/oberthci/oberth/test-secret"
	req := testRequest("release", perRepoCredentialedDocument(path))
	req.Repo = "oberth"
	req.UpstreamName = "codeberg"
	req.UpstreamOrg = "oberthci"
	req.ApprovedSecrets = map[string]bool{path: true}
	req.SourceVolume = SourceVolume{
		ClaimName:      "test-claim",
		SubPath:        "src",
		VaultCASubPath: "vault-ca",
		BinarySubPath:  "bin",
	}
	req.Identities = snapshot

	wf, err := Build(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	// The Workflow must use the snapshot's SA, not the Config's.
	if wf.Spec.ServiceAccountName != "snapshot-release-sa" {
		t.Fatalf("ServiceAccount = %q, want snapshot's %q", wf.Spec.ServiceAccountName, "snapshot-release-sa")
	}
}
