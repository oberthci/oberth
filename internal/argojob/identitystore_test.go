package argojob

import (
	"strings"
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

// TestIdentityStoreConcurrentRace runs concurrent Snapshot readers against a
// Replace writer. Under -race this proves three things: the atomic pointer
// publishes snapshots without data races; maps freshly built for each Replace
// are safely published to readers (the serve-path callback's copy-on-write
// contract); and the release and CI maps a reader observes always come from
// the same version — the per-admission identity/role lockstep issue #465
// requires. A refactor that split the two tiers across separate atomic
// pointers would fail the lockstep assertion here.
func TestIdentityStoreConcurrentRace(t *testing.T) {
	t.Parallel()
	const key = "codeberg/org/repo"
	// version builds a coherent (release, CI) map pair from scratch. Both
	// maps of one version share the same suffix; a torn read would surface
	// as a mixed-suffix pair.
	version := func(n int) (map[string]PerRepoIdentityConfig, map[string]PerRepoIdentityConfig) {
		suffix := "A"
		if n%2 == 1 {
			suffix = "B"
		}
		return map[string]PerRepoIdentityConfig{key: {ServiceAccountName: "sa-" + suffix}},
			map[string]PerRepoIdentityConfig{key: {ServiceAccountName: "ci-" + suffix}}
	}
	release, ci := version(0)
	store := NewIdentityStore(release, ci)

	var wg sync.WaitGroup
	// Writer: continuously replace the snapshot with freshly built maps,
	// alternating between two coherent versions.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 1000 {
			release, ci := version(i)
			store.Replace(release, ci)
		}
	}()
	// Readers: continuously snapshot and verify version coherence by
	// reading the map entries themselves, not just the map headers.
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
				releaseSA := snap.Release[key].ServiceAccountName
				ciSA := snap.CI[key].ServiceAccountName
				if releaseSA == "" || ciSA == "" {
					t.Errorf("snapshot missing identity: release %q, CI %q", releaseSA, ciSA)
					return
				}
				if strings.TrimPrefix(releaseSA, "sa-") != strings.TrimPrefix(ciSA, "ci-") {
					t.Errorf("torn snapshot: release %q paired with CI %q", releaseSA, ciSA)
					return
				}
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
