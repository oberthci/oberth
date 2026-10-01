package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestAdministrativeRegistrationsAreAtomicWithAudit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 21, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	upstream, err := database.RegisterUpstream(context.Background(), "admin@localhost", model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := database.CreateTokenCredential(context.Background(), model.TokenCredentialSpec{Name: "agent@host", Digest: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := database.RegisterUplink(context.Background(), "admin@localhost", model.UplinkSpec{
		Fingerprint: "SHA256:key", Identity: "agent@host", TokenCredentialID: pending.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if upstream.ID <= 0 || uplink.TokenCredentialID != pending.ID || uplink.AuthActor != "admin@localhost" {
		t.Fatalf("upstream/uplink = %+v / %+v", upstream, uplink)
	}
	var actions int
	if err := database.db.QueryRowContext(context.Background(), `
SELECT count(*) FROM audit_actions WHERE action IN ('upstream.register', 'uplink.register')`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions != 2 {
		t.Fatalf("administrative audit actions = %d", actions)
	}
}

func TestRegisterUpstreamRejectsOrgAliasing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	ctx := context.Background()

	// Upstream A claims org "acme" on one forge.
	if _, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
		Name: "github-acme", Kind: "ssh", BaseURL: "ssh://git@github.com/acme",
	}); err != nil {
		t.Fatalf("register first upstream: %v", err)
	}

	// Upstream B: different name, different host/scheme, SAME trailing segment
	// -> same Org() "acme" -> would alias oberth/upstream/acme/*. Must be
	// rejected with ErrInvalid before any row is created (issue #217).
	_, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
		Name: "gitlab-acme", Kind: "https", BaseURL: "https://gitlab.com/acme",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("org-aliasing registration error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "acme") || !strings.Contains(err.Error(), "github-acme") {
		t.Fatalf("error must name the org and the conflicting upstream: %v", err)
	}

	// The rejected upstream must not have been created (fail closed, atomic).
	if _, err := database.UpstreamByName(ctx, "gitlab-acme"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected upstream must not exist: got err=%v", err)
	}

	// A genuinely distinct org still registers.
	if _, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
		Name: "github-other", Kind: "ssh", BaseURL: "ssh://git@github.com/other",
	}); err != nil {
		t.Fatalf("register distinct-org upstream: %v", err)
	}

	// A base URL that derives an empty org is rejected too.
	if _, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
		Name: "empty-org", Kind: "ssh", BaseURL: "/",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty-org registration error = %v, want ErrInvalid", err)
	}

	// Exactly the two legitimate upstreams exist; no register audit action for
	// either rejected attempt.
	all, err := database.ListUpstreams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("upstream count = %d, want 2 (aliased and empty-org rejected)", len(all))
	}
	var registered int
	if err := database.db.QueryRowContext(ctx, `
SELECT count(*) FROM audit_actions WHERE action = 'upstream.register'`).Scan(&registered); err != nil {
		t.Fatal(err)
	}
	if registered != 2 {
		t.Fatalf("upstream.register audit actions = %d, want 2", registered)
	}
}

func TestRegisterUplinkAdminRoundTrips(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	database := testStore(t, &now)

	// Register a non-admin uplink.
	pending1, err := database.CreateTokenCredential(context.Background(), model.TokenCredentialSpec{
		Name: "regular@host", Digest: make([]byte, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	regular, err := database.RegisterUplink(context.Background(), "admin@localhost", model.UplinkSpec{
		Fingerprint: "SHA256:regular", Identity: "regular@host", TokenCredentialID: pending1.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if regular.Admin {
		t.Fatal("non-admin uplink should have Admin=false")
	}

	// Register an admin uplink.
	digest2 := make([]byte, 32)
	digest2[0] = 1
	pending2, err := database.CreateTokenCredential(context.Background(), model.TokenCredentialSpec{
		Name: "admin@host", Digest: digest2,
	})
	if err != nil {
		t.Fatal(err)
	}
	admin, err := database.RegisterUplink(context.Background(), "admin@localhost", model.UplinkSpec{
		Fingerprint: "SHA256:admin", Identity: "admin@host", TokenCredentialID: pending2.ID,
		Admin: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !admin.Admin {
		t.Fatal("admin uplink should have Admin=true")
	}

	// Verify round-trip through UplinkByFingerprint.
	readback, err := database.UplinkByFingerprint(context.Background(), "SHA256:admin")
	if err != nil {
		t.Fatal(err)
	}
	if !readback.Admin {
		t.Fatal("admin flag lost on UplinkByFingerprint round-trip")
	}

	readbackRegular, err := database.UplinkByFingerprint(context.Background(), "SHA256:regular")
	if err != nil {
		t.Fatal(err)
	}
	if readbackRegular.Admin {
		t.Fatal("non-admin flag corrupted on UplinkByFingerprint round-trip")
	}

	// Verify round-trip through ListUplinkStatuses.
	statuses, err := database.ListUplinkStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	adminCount := 0
	for _, status := range statuses {
		if status.Admin {
			adminCount++
			if status.Fingerprint != "SHA256:admin" {
				t.Fatalf("unexpected admin uplink fingerprint = %s", status.Fingerprint)
			}
		}
	}
	if adminCount != 1 {
		t.Fatalf("admin uplinks in list = %d, want 1", adminCount)
	}

	// Verify audit details include admin field.
	var details string
	if err := database.db.QueryRowContext(context.Background(), `
SELECT details FROM audit_actions WHERE action = 'uplink.register' ORDER BY id DESC LIMIT 1`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !containsString(details, `"admin":true`) {
		t.Fatalf("audit details missing admin flag: %s", details)
	}
}

func TestUplinkRotationRevokesDisplacedCredentialAtomically(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	ctx := context.Background()

	digest1 := make([]byte, 32)
	digest1[0] = 0x01
	cred1, err := database.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "agent@host", Digest: digest1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.RegisterUplink(ctx, "admin@localhost", model.UplinkSpec{
		Fingerprint: "SHA256:rotate", Identity: "agent@host", TokenCredentialID: cred1.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ActivateTokenCredential(ctx, cred1.ID); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Second)
	digest2 := make([]byte, 32)
	digest2[0] = 0x02
	cred2, err := database.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "agent@host-rotated", Digest: digest2})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := database.RegisterUplink(ctx, "admin@localhost", model.UplinkSpec{
		Fingerprint: "SHA256:rotate", Identity: "agent@host", TokenCredentialID: cred2.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rotated.TokenCredentialID != cred2.ID {
		t.Fatalf("rotated uplink should point to new credential, got %s", rotated.TokenCredentialID)
	}

	var revokedAt *int64
	if err := database.db.QueryRowContext(ctx, `SELECT revoked_at FROM token_credentials WHERE id = ?`, cred1.ID).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if revokedAt == nil {
		t.Fatal("displaced credential should be revoked after rotation")
	}

	var details string
	if err := database.db.QueryRowContext(ctx, `
SELECT details FROM audit_actions WHERE action = 'uplink.register' ORDER BY id DESC LIMIT 1`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !containsString(details, cred1.ID) {
		t.Fatalf("rotation audit should record displaced credential ID, got %s", details)
	}
}

func containsString(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || len(haystack) > 0 && containsSubstring(haystack, needle))
}

func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRegisterUpstreamRejectsReservedNames(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	ctx := context.Background()

	// Guard 1: reserved names that would alias security boundaries.
	for _, name := range []string{"release", "data", "upstream", "sys", "metadata", "receive-outbox"} {
		_, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
			Name: name, Kind: "ssh", BaseURL: "ssh://git@example.com/" + name,
		})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("reserved upstream name %q: error = %v, want ErrInvalid", name, err)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("reserved upstream name %q: error must say 'reserved': %v", name, err)
		}
	}

	// A non-reserved name registers fine.
	if _, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	}); err != nil {
		t.Fatalf("register non-reserved name: %v", err)
	}
}

func TestRegisterUpstreamRejectsNameOrgDisjointness(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	ctx := context.Background()

	// Register upstream "codeberg" with org "acme" (from base URL).
	if _, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	}); err != nil {
		t.Fatalf("register first upstream: %v", err)
	}

	// Guard 2: namespace disjointness — a second upstream whose NAME matches
	// the first upstream's ORG must be rejected.
	_, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
		Name: "acme", Kind: "ssh", BaseURL: "ssh://git@github.com/different-org",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("name-org disjointness: error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "collides") {
		t.Fatalf("name-org disjointness: error must say 'collides': %v", err)
	}

	// Verify no leaked upstream was created.
	all, err := database.ListUpstreams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("upstream count = %d, want 1", len(all))
	}
}

// TestSameNameDifferentUpstreamAllowedAfterG3 verifies that the compound
// UNIQUE(upstream_id, name) constraint (G3 canonical persistence) allows
// same-name repositories under different upstreams while still rejecting
// duplicates under the same upstream.
func TestSameNameDifferentUpstreamAllowedAfterG3(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 3, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	ctx := context.Background()

	upstream1, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	upstream2, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/oberthci",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Register terraform under codeberg.
	if _, err := database.RegisterRepository(ctx, "admin@localhost", model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream1.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("register terraform under codeberg: %v", err)
	}

	// G3: same name under a different upstream is now allowed.
	repo2, err := database.RegisterRepository(ctx, "admin@localhost", model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream2.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatalf("same-name repo under different upstream should succeed after G3: %v", err)
	}
	if repo2.UpstreamID != upstream2.ID {
		t.Fatalf("second terraform repo upstream = %d, want %d", repo2.UpstreamID, upstream2.ID)
	}

	// Same name under the same upstream must still fail.
	_, err = database.RegisterRepository(ctx, "admin@localhost", model.RepositorySpec{
		Name: "terraform", UpstreamID: upstream1.ID, DefaultBranch: "main",
	})
	if err == nil {
		t.Fatal("duplicate name under same upstream should fail")
	}

	// Bare-name lookup should be ambiguous now.
	_, err = database.RepositoryByName(ctx, "terraform")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("bare-name lookup for ambiguous repo = %v, want ErrAmbiguous", err)
	}

	// Org-qualified lookup should resolve correctly.
	found, err := database.RepositoryByName(ctx, "oberthci/terraform")
	if err != nil {
		t.Fatalf("org-qualified lookup: %v", err)
	}
	if found.ID != repo2.ID {
		t.Fatalf("org-qualified lookup returned repo %d, want %d", found.ID, repo2.ID)
	}

	// Fully-qualified lookup should resolve correctly.
	found, err = database.RepositoryByName(ctx, "codeberg/acme/terraform")
	if err != nil {
		t.Fatalf("fully-qualified lookup: %v", err)
	}
	if found.UpstreamID != upstream1.ID {
		t.Fatalf("fully-qualified lookup upstream = %d, want %d", found.UpstreamID, upstream1.ID)
	}
}

func TestRegisterUpstreamRejectsOrgCollidingWithExistingName(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 4, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	ctx := context.Background()

	// Register upstream named "codeberg".
	if _, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	}); err != nil {
		t.Fatalf("register first upstream: %v", err)
	}

	// G2 reverse: adding a second upstream whose org (from base URL) is
	// "codeberg" must be rejected because it collides with the existing
	// upstream NAME "codeberg". A 2-segment path "codeberg/repo" would
	// be ambiguous: upstream name or org identity?
	_, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
		Name: "github-mirror", Kind: "ssh", BaseURL: "ssh://git@github.com/codeberg",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("G2 reverse: error = %v, want ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "collides") && !strings.Contains(err.Error(), "codeberg") {
		t.Fatalf("G2 reverse: error must name the collision: %v", err)
	}

	// Verify no leaked upstream was created.
	all, err := database.ListUpstreams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("upstream count = %d, want 1", len(all))
	}
}

func TestRegisterUpstreamRejectsInvalidCharset(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)
	database := testStore(t, &now)
	ctx := context.Background()

	// Guard 1 (charset): upstream names must match repoPattern.
	for _, name := range []string{"-bad", "..", "a/b", "bad name"} {
		_, err := database.RegisterUpstream(ctx, "admin@localhost", model.UpstreamSpec{
			Name: name, Kind: "ssh", BaseURL: "ssh://git@example.com/org",
		})
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid charset upstream name %q: error = %v, want ErrInvalid", name, err)
		}
	}
}
