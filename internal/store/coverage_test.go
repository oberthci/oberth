package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// ---------------------------------------------------------------------------
// Utility helpers — validOID, unixNano, fromUnixNano, nullableTime,
// translateNotFound, requireChanged, pageLimit, isHexPrefix, nullableUnixNano
// ---------------------------------------------------------------------------

func TestValidOID(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true},                         // 40-char SHA-1
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true}, // 64-char SHA-256
		{"", false}, // empty
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},                          // 39 chars
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},                        // 41 chars
		{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", false},                         // uppercase
		{"gggggggggggggggggggggggggggggggggggggggg", false},                         // non-hex
		{"aaaaaaaaaaaaaaaaaaaaaaaa aaaaaaaaaaaaaaaaa", false},                       // space
		{"0123456789abcdef0123456789abcdef01234567", true},                          // mixed hex
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", true},  // 64 mixed
		{"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeF", false}, // trailing uppercase
	} {
		if got := validOID(tc.value); got != tc.want {
			t.Errorf("validOID(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestUnixNanoRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 14, 30, 0, 123456789, time.UTC)
	nano := unixNano(now)
	if nano <= 0 {
		t.Fatalf("unixNano returned non-positive: %d", nano)
	}
	back := fromUnixNano(nano)
	if !back.Equal(now) {
		t.Fatalf("round-trip: %v != %v", back, now)
	}
}

func TestNullableTime(t *testing.T) {
	t.Parallel()
	if result := nullableTime(sql.NullInt64{Valid: false}); result != nil {
		t.Fatalf("nullableTime(invalid) = %v, want nil", result)
	}
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	nano := unixNano(now)
	result := nullableTime(sql.NullInt64{Int64: nano, Valid: true})
	if result == nil || !result.Equal(now) {
		t.Fatalf("nullableTime(valid) = %v, want %v", result, now)
	}
}

func TestNullableUnixNano(t *testing.T) {
	t.Parallel()
	if result := nullableUnixNano(nil); result != nil {
		t.Fatalf("nullableUnixNano(nil) = %v, want nil", result)
	}
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	result := nullableUnixNano(&now)
	if result == nil {
		t.Fatal("nullableUnixNano(&now) = nil")
	}
	if v, ok := result.(int64); !ok || v != unixNano(now) {
		t.Fatalf("nullableUnixNano(&now) = %v, want %d", result, unixNano(now))
	}
}

func TestTranslateNotFound(t *testing.T) {
	t.Parallel()
	result := translateNotFound("widget", sql.ErrNoRows)
	if !errors.Is(result, ErrNotFound) {
		t.Fatalf("translateNotFound(ErrNoRows) = %v, want ErrNotFound", result)
	}
	other := errors.New("database error")
	if result := translateNotFound("widget", other); !errors.Is(result, other) {
		t.Fatalf("translateNotFound(other) = %v, want %v", result, other)
	}
}

type mockResult struct {
	affected int64
	affErr   error
}

func (m mockResult) LastInsertId() (int64, error) { return 0, nil }
func (m mockResult) RowsAffected() (int64, error) { return m.affected, m.affErr }

func TestRequireChanged(t *testing.T) {
	t.Parallel()
	if err := requireChanged("thing", mockResult{affected: 1}); err != nil {
		t.Fatalf("requireChanged(1) = %v", err)
	}
	if err := requireChanged("thing", mockResult{affected: 0}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("requireChanged(0) = %v, want ErrNotFound", err)
	}
	mockErr := errors.New("driver error")
	if err := requireChanged("thing", mockResult{affErr: mockErr}); !errors.Is(err, mockErr) {
		t.Fatalf("requireChanged(err) = %v, want %v", err, mockErr)
	}
}

func TestPageLimit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input int
		want  int
	}{
		{0, defaultPageSize},
		{-1, defaultPageSize},
		{1, 1},
		{50, 50},
		{100, 100},
		{101, maxPageSize},
		{999, maxPageSize},
	} {
		if got := pageLimit(tc.input); got != tc.want {
			t.Errorf("pageLimit(%d) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestIsHexPrefix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0123456789abcdef", true},
		{"abc", true},
		{"ABC", false}, // uppercase
		{"0g", false},
		{"a b", false},
	} {
		if got := isHexPrefix(tc.value); got != tc.want {
			t.Errorf("isHexPrefix(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Repository lookup — Repository, RepositoryByName
// ---------------------------------------------------------------------------

func TestRepositoryLookupByIDAndName(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	byID, err := s.Repository(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if byID.Name != repo.Name || byID.ID != repo.ID {
		t.Fatalf("Repository(id) = %#v", byID)
	}

	byName, err := s.RepositoryByName(ctx, repo.Name)
	if err != nil {
		t.Fatal(err)
	}
	if byName.ID != repo.ID || byName.Name != repo.Name {
		t.Fatalf("RepositoryByName(%q) = %#v", repo.Name, byName)
	}

	if _, err := s.Repository(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Repository(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.RepositoryByName(ctx, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RepositoryByName(missing) = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Admin — RegisterRepository, RemoveUpstream, RemoveUplink
// ---------------------------------------------------------------------------

func TestRegisterRepositoryAtomicWithAudit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream, err := s.RegisterUpstream(ctx, "admin@host", model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.RegisterRepository(ctx, "admin@host", model.RepositorySpec{
		Name: "test-repo", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if repo.Name != "test-repo" || repo.UpstreamID != upstream.ID || repo.DefaultBranch != "main" {
		t.Fatalf("registered repository = %#v", repo)
	}

	var actions int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_actions WHERE action = 'repository.register'`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if actions != 1 {
		t.Fatalf("repository.register audit actions = %d, want 1", actions)
	}

	// Validation: empty actor, missing fields.
	if _, err := s.RegisterRepository(ctx, "", model.RepositorySpec{
		Name: "r", UpstreamID: upstream.ID, DefaultBranch: "main",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor error = %v, want ErrInvalid", err)
	}
	if _, err := s.RegisterRepository(ctx, "admin@host", model.RepositorySpec{
		Name: "", UpstreamID: upstream.ID, DefaultBranch: "main",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty name error = %v, want ErrInvalid", err)
	}
}

func TestRemoveUpstreamAtomicWithAudit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream, err := s.RegisterUpstream(ctx, "admin@host", model.UpstreamSpec{
		Name: "removable", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.RegisterRepository(ctx, "admin@host", model.RepositorySpec{
		Name: "mapped-repo", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = repo

	removed, err := s.RemoveUpstream(ctx, "admin@host", "removable")
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "mapped-repo" {
		t.Fatalf("removed repositories = %v, want [mapped-repo]", removed)
	}
	// Verify upstream is gone.
	if _, err := s.UpstreamByName(ctx, "removable"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed upstream lookup = %v, want ErrNotFound", err)
	}

	var auditCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_actions WHERE action = 'upstream.remove'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("upstream.remove audit actions = %d, want 1", auditCount)
	}

	// Validation.
	if _, err := s.RemoveUpstream(ctx, "", "foo"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor = %v, want ErrInvalid", err)
	}
	if _, err := s.RemoveUpstream(ctx, "admin", "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing upstream = %v, want ErrNotFound", err)
	}
}

func TestRemoveRepositoryAtomicWithAudit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream, err := s.RegisterUpstream(ctx, "admin@host", model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRepository(ctx, "admin@host", model.RepositorySpec{
		Name: "removable-repo", UpstreamID: upstream.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := s.RemoveRepository(ctx, "admin@host", "removable-repo")
	if err != nil {
		t.Fatal(err)
	}
	if removed.Name != "removable-repo" || removed.UpstreamName != "github" {
		t.Fatalf("removed = %+v", removed)
	}

	// Verify repository is gone.
	if _, err := s.RepositoryByName(ctx, "removable-repo"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed repository lookup = %v, want ErrNotFound", err)
	}

	// Verify audit action was recorded.
	var auditCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_actions WHERE action = 'repository.remove'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("repository.remove audit actions = %d, want 1", auditCount)
	}

	// Validation.
	if _, err := s.RemoveRepository(ctx, "", "foo"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor = %v, want ErrInvalid", err)
	}
	if _, err := s.RemoveRepository(ctx, "admin", "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing repository = %v, want ErrNotFound", err)
	}
}

func TestRemoveRepositoryRefusesInFlightRuns(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream, err := s.RegisterUpstream(ctx, "admin@host", model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.RegisterRepository(ctx, "admin@host", model.RepositorySpec{
		Name: "busy-repo", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main",
		SHA: strings.Repeat("b", 40), Actor: "test@host", Trigger: "push",
	}); err != nil {
		t.Fatal(err)
	}

	_, err = s.RemoveRepository(ctx, "admin@host", "busy-repo")
	if err == nil || !strings.Contains(err.Error(), "in-flight") {
		t.Fatalf("expected in-flight rejection, got %v", err)
	}
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState, got %v", err)
	}
}

func TestRemoveRepositoryRefusesImmutableHistory(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	upstream, err := s.RegisterUpstream(ctx, "admin@host", model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.RegisterRepository(ctx, "admin@host", model.RepositorySpec{
		Name: "historied-repo", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	enqueued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "main", strings.Repeat("c", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{Status: model.RunPassed}); err != nil {
		t.Fatal(err)
	}

	// The run is terminal, so the in-flight guard passes; the FK constraint
	// is what must refuse. History rows are immutable evidence and are never
	// cascaded away — the distinguishing invariant of repository removal.
	_, err = s.RemoveRepository(ctx, "admin@host", "historied-repo")
	if err == nil || !strings.Contains(err.Error(), "immutable CI history") {
		t.Fatalf("expected immutable-history rejection, got %v", err)
	}
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState, got %v", err)
	}

	// The refused removal must roll back completely: the repository row
	// survives and no repository.remove audit action is recorded.
	if _, err := s.RepositoryByName(ctx, "historied-repo"); err != nil {
		t.Fatalf("repository must survive refused removal: %v", err)
	}
	var auditCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_actions WHERE action = 'repository.remove'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 0 {
		t.Fatalf("refused removal recorded %d repository.remove audit actions, want 0", auditCount)
	}
}

func TestRemoveUplinkAtomicWithAudit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	digest := make([]byte, 32)
	digest[0] = 0x99
	cred, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "removable@host", Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RegisterUplink(ctx, "admin@host", model.UplinkSpec{
		Fingerprint: "SHA256:removable", Identity: "removable@host", TokenCredentialID: cred.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTokenCredential(ctx, cred.ID); err != nil {
		t.Fatal(err)
	}

	removed, err := s.RemoveUplink(ctx, "admin@host", "removable@host", "")
	if err != nil {
		t.Fatal(err)
	}
	if removed.Identity != "removable@host" || removed.Fingerprint != "SHA256:removable" {
		t.Fatalf("removed uplink = %#v", removed)
	}
	// Credential should be revoked.
	readback, err := s.TokenCredentialByDigest(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if readback.RevokedAt == nil {
		t.Fatal("credential should be revoked after uplink removal")
	}

	var auditCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_actions WHERE action = 'uplink.remove'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("uplink.remove audit actions = %d, want 1", auditCount)
	}

	// Validation.
	if _, err := s.RemoveUplink(ctx, "", "foo", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor = %v, want ErrInvalid", err)
	}
	if _, err := s.RemoveUplink(ctx, "admin", "nonexistent", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing uplink = %v, want ErrNotFound", err)
	}
}

func TestRemoveUplinkAmbiguousRequiresFingerprint(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Create two uplinks with the same identity but different fingerprints.
	digest1 := make([]byte, 32)
	digest1[0] = 0xA1
	cred1, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "ambig@host-1", Digest: digest1})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RegisterUplink(ctx, "admin@host", model.UplinkSpec{
		Fingerprint: "SHA256:ambig1", Identity: "ambig@host", TokenCredentialID: cred1.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	digest2 := make([]byte, 32)
	digest2[0] = 0xA2
	cred2, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "ambig@host-2", Digest: digest2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.RegisterUplink(ctx, "admin@host", model.UplinkSpec{
		Fingerprint: "SHA256:ambig2", Identity: "ambig@host", TokenCredentialID: cred2.ID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Remove without fingerprint: ambiguous.
	if _, err := s.RemoveUplink(ctx, "admin@host", "ambig@host", ""); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous remove = %v, want ErrAmbiguous", err)
	}
	// Remove with fingerprint: succeeds.
	removed, err := s.RemoveUplink(ctx, "admin@host", "ambig@host", "SHA256:ambig1")
	if err != nil {
		t.Fatal(err)
	}
	if removed.Fingerprint != "SHA256:ambig1" {
		t.Fatalf("removed fingerprint = %s, want SHA256:ambig1", removed.Fingerprint)
	}
}

// ---------------------------------------------------------------------------
// Audit — SoleAuditAction, VerifyAuditStateSince, AuditAnchors,
// VerifyAuditAnchorReference/References, auditHashIndex
// ---------------------------------------------------------------------------

func TestSoleAuditAction(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Empty chain: SoleAuditAction fails.
	if _, err := s.SoleAuditAction(ctx); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("empty chain SoleAuditAction = %v, want ErrInvalidState", err)
	}

	// One action: succeeds.
	action, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test.sole", ResourceType: "test", ResourceID: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	sole, err := s.SoleAuditAction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sole.ID != action.ID || sole.Action != "test.sole" {
		t.Fatalf("SoleAuditAction = %#v, want id=%d", sole, action.ID)
	}

	// Two actions: SoleAuditAction fails.
	now = now.Add(time.Second)
	if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test.second", ResourceType: "test", ResourceID: "2",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SoleAuditAction(ctx); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("two-action chain SoleAuditAction = %v, want ErrInvalidState", err)
	}
}

func TestVerifyAuditStateSince(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// First action.
	if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test.first", ResourceType: "test", ResourceID: "1",
	}); err != nil {
		t.Fatal(err)
	}
	head1, err := s.VerifyAuditState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head1.ID != 1 || len(head1.SHA256) != sha256.Size {
		t.Fatalf("first head = %#v", head1)
	}

	// Second action.
	now = now.Add(time.Second)
	if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test.second", ResourceType: "test", ResourceID: "2",
	}); err != nil {
		t.Fatal(err)
	}

	// Incremental verification from head1.
	head2, err := s.VerifyAuditStateSince(ctx, head1.ID, head1.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if head2.ID != 2 {
		t.Fatalf("incremental head = %d, want 2", head2.ID)
	}

	// Fallback to full verification when sinceID is 0.
	headFull, err := s.VerifyAuditStateSince(ctx, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if headFull.ID != 2 {
		t.Fatalf("fallback head = %d, want 2", headFull.ID)
	}
}

func TestAuditAnchorsAndVerification(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Create an audit action to anchor to.
	if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test.anchor", ResourceType: "test", ResourceID: "1",
	}); err != nil {
		t.Fatal(err)
	}
	head, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// No anchors yet.
	anchors, err := s.AuditAnchors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors) != 0 {
		t.Fatalf("expected no anchors, got %d", len(anchors))
	}

	// LatestAuditAnchor returns not-found when none exist.
	if _, err := s.LatestAuditAnchor(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty LatestAuditAnchor = %v, want ErrNotFound", err)
	}

	// Record an anchor.
	anchorTime := now.Add(time.Minute)
	anchor, err := s.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID:     head.ID,
		AuditSHA256: head.SHA256,
		TSAURL:      "https://timestamp.example.com",
		Receipt:     []byte("signed-receipt-data"),
		AnchoredAt:  anchorTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if anchor.AuditID != head.ID || anchor.TSAURL != "https://timestamp.example.com" {
		t.Fatalf("recorded anchor = %#v", anchor)
	}

	// LatestAuditAnchor returns the one we created.
	latest, err := s.LatestAuditAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != anchor.ID {
		t.Fatalf("latest anchor ID = %d, want %d", latest.ID, anchor.ID)
	}

	// AuditAnchors returns the list.
	anchors, err = s.AuditAnchors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors) != 1 || anchors[0].ID != anchor.ID {
		t.Fatalf("anchors = %#v", anchors)
	}

	// VerifyAuditAnchorReference succeeds.
	if err := s.VerifyAuditAnchorReference(ctx, anchor); err != nil {
		t.Fatalf("VerifyAuditAnchorReference = %v", err)
	}

	// VerifyAuditAnchorReferences succeeds.
	if err := s.VerifyAuditAnchorReferences(ctx, anchors); err != nil {
		t.Fatalf("VerifyAuditAnchorReferences = %v", err)
	}

	// VerifyAuditAnchorReferences with a bad anchor.
	bad := model.AuditAnchor{
		AuditID:     head.ID,
		AuditSHA256: make([]byte, sha256.Size), // wrong hash
	}
	if err := s.VerifyAuditAnchorReferences(ctx, []model.AuditAnchor{bad}); err == nil {
		t.Fatal("VerifyAuditAnchorReferences(bad) should fail")
	}
}

// ---------------------------------------------------------------------------
// RunningRunsWithJobs
// ---------------------------------------------------------------------------

func TestRunningRunsWithJobs(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	// Empty: no running runs.
	runs, err := s.RunningRunsWithJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected no running runs, got %d", len(runs))
	}

	// Enqueue and claim a run.
	enqueued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/jobs", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}

	// Running but no job name: not returned.
	runs, err = s.RunningRunsWithJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected no runs without job name, got %d", len(runs))
	}

	// Set job name.
	if _, err := s.SetRunJobName(ctx, enqueued.ID, "oberth-job-test"); err != nil {
		t.Fatal(err)
	}

	// Now it shows up.
	runs, err = s.RunningRunsWithJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != enqueued.ID || runs[0].JobName != "oberth-job-test" {
		t.Fatalf("running runs with jobs = %#v", runs)
	}

	// Finish the run: no longer returned.
	now = now.Add(time.Minute)
	if _, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{
		Status: model.RunPassed, Phase: "build",
	}); err != nil {
		t.Fatal(err)
	}
	runs, err = s.RunningRunsWithJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("expected no runs after finish, got %d", len(runs))
	}
}

// ---------------------------------------------------------------------------
// Workspace cleanup — RunWorkspaceCleanupEligible,
// PromotionWorkspaceCleanupEligible, RunWorkspaceCleanupCandidates,
// PromotionWorkspaceCleanupCandidates, scanWorkspaceOwnerIDs
// ---------------------------------------------------------------------------

func TestRunWorkspaceCleanupEligible(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	// Validation: empty ID.
	if _, err := s.RunWorkspaceCleanupEligible(ctx, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty ID = %v, want ErrInvalid", err)
	}

	// Queued run: not eligible.
	enqueued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/ws", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := s.RunWorkspaceCleanupEligible(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if eligible {
		t.Fatal("queued run should not be eligible")
	}

	// Running run: not eligible.
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	eligible, err = s.RunWorkspaceCleanupEligible(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if eligible {
		t.Fatal("running run should not be eligible")
	}

	// Finish run: now eligible.
	now = now.Add(time.Minute)
	if _, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{
		Status: model.RunPassed, Phase: "build",
	}); err != nil {
		t.Fatal(err)
	}
	eligible, err = s.RunWorkspaceCleanupEligible(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !eligible {
		t.Fatal("passed run should be eligible")
	}

	// Nonexistent run: not eligible but no error.
	eligible, err = s.RunWorkspaceCleanupEligible(ctx, "nonexistent-id")
	if err != nil {
		t.Fatal(err)
	}
	if eligible {
		t.Fatal("nonexistent run should not be eligible")
	}
}

func TestPromotionWorkspaceCleanupEligible(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	// Validation: empty ID.
	if _, err := s.PromotionWorkspaceCleanupEligible(ctx, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty ID = %v, want ErrInvalid", err)
	}

	// Pending promotion: not eligible.
	promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/ws", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "main", PreviousSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ResultSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "SHA256:operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := s.PromotionWorkspaceCleanupEligible(ctx, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if eligible {
		t.Fatal("pending promotion should not be eligible")
	}

	// Finish promotion: now eligible.
	now = now.Add(time.Second)
	if _, err := s.FinishPromotion(ctx, promotion.ID, model.PromotionFailed, "", "tests failed"); err != nil {
		t.Fatal(err)
	}
	eligible, err = s.PromotionWorkspaceCleanupEligible(ctx, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !eligible {
		t.Fatal("failed promotion should be eligible")
	}
}

func TestRunWorkspaceCleanupCandidates(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	// Create and finish two runs.
	var finishedIDs []string
	for i, sha := range []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	} {
		now = now.Add(time.Duration(i) * time.Second)
		enqueued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/candidates", sha))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimNextRun(ctx); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
		if _, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{
			Status: model.RunFailed, Phase: "test", Error: "fail",
		}); err != nil {
			t.Fatal(err)
		}
		finishedIDs = append(finishedIDs, enqueued.ID)
	}

	candidates, err := s.RunWorkspaceCleanupCandidates(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(candidates))
	}

	// Pagination: get one at a time.
	page1, err := s.RunWorkspaceCleanupCandidates(ctx, "", 1)
	if err != nil || len(page1) != 1 {
		t.Fatalf("page1 = %v, %v", page1, err)
	}
	page2, err := s.RunWorkspaceCleanupCandidates(ctx, page1[0], 1)
	if err != nil || len(page2) != 1 {
		t.Fatalf("page2 = %v, %v", page2, err)
	}
	if page1[0] == page2[0] {
		t.Fatal("pages returned the same candidate")
	}

	// Queued run: not a candidate.
	now = now.Add(time.Second)
	if _, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/queued", "cccccccccccccccccccccccccccccccccccccccc")); err != nil {
		t.Fatal(err)
	}
	all, err := s.RunWorkspaceCleanupCandidates(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("candidates with queued run = %d, want 2", len(all))
	}
	_ = finishedIDs
}

func TestPromotionWorkspaceCleanupCandidates(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	// Create and finish two promotions.
	for i, sha := range []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	} {
		now = now.Add(time.Duration(i) * time.Second)
		promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{
			RepoID: repo.ID, SourceBranch: "feature/pc", SourceSHA: sha,
			TargetRef: "main", PreviousSHA: "0000000000000000000000000000000000000000",
			ResultSHA: sha, Actor: "SHA256:operator",
		})
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
		if _, err := s.FinishPromotion(ctx, promotion.ID, model.PromotionFailed, "", "fail"); err != nil {
			t.Fatal(err)
		}
	}

	candidates, err := s.PromotionWorkspaceCleanupCandidates(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("promotion candidates = %d, want 2", len(candidates))
	}

	// Pending promotion: not a candidate.
	now = now.Add(time.Second)
	if _, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/pending", SourceSHA: "cccccccccccccccccccccccccccccccccccccccc",
		TargetRef: "main", PreviousSHA: "0000000000000000000000000000000000000000",
		ResultSHA: "cccccccccccccccccccccccccccccccccccccccc", Actor: "SHA256:operator",
	}); err != nil {
		t.Fatal(err)
	}
	all, err := s.PromotionWorkspaceCleanupCandidates(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("candidates with pending = %d, want 2", len(all))
	}
}

// ---------------------------------------------------------------------------
// Token credential lifecycle — ActivateTokenCredential, scanTokenCredential
// ---------------------------------------------------------------------------

func TestActivateTokenCredentialRequiresUplink(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	digest := make([]byte, 32)
	digest[0] = 0xBB
	cred, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "orphan@host", Digest: digest})
	if err != nil {
		t.Fatal(err)
	}

	// Activation without an uplink fails.
	if _, err := s.ActivateTokenCredential(ctx, cred.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("activate without uplink = %v, want ErrNotFound", err)
	}

	// Create uplink.
	if _, err := s.UpsertUplink(ctx, model.UplinkSpec{
		Fingerprint: "SHA256:activate-test", Identity: "orphan@host",
		TokenCredentialID: cred.ID, AuthActor: "token:" + cred.ID,
	}); err != nil {
		t.Fatal(err)
	}

	// Now activation succeeds.
	activated, err := s.ActivateTokenCredential(ctx, cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if activated.ActivatedAt == nil {
		t.Fatal("activated credential should have ActivatedAt set")
	}
	if activated.RevokedAt != nil {
		t.Fatal("activated credential should not be revoked")
	}

	// Double activation fails.
	if _, err := s.ActivateTokenCredential(ctx, cred.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double activation = %v, want ErrNotFound", err)
	}

	// Nonexistent credential activation fails.
	if _, err := s.ActivateTokenCredential(ctx, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nonexistent activation = %v, want ErrNotFound", err)
	}
}

func TestScanTokenCredentialRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	digest := make([]byte, 32)
	digest[0] = 0xCC
	cred, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "scan@host", Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	// The scanTokenCredential path is exercised through TokenCredentialByDigest.
	readback, err := s.TokenCredentialByDigest(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if readback.ID != cred.ID || readback.Name != "scan@host" {
		t.Fatalf("scanTokenCredential round-trip = %#v", readback)
	}
	// Pending credential has RevokedAt set (pending state).
	if readback.RevokedAt == nil {
		t.Fatal("pending credential should have RevokedAt placeholder")
	}
	if readback.ActivatedAt != nil {
		t.Fatal("pending credential should not have ActivatedAt")
	}
}

// ---------------------------------------------------------------------------
// PlanPromotion — full lifecycle including idempotency and rejection
// ---------------------------------------------------------------------------

func TestPlanPromotionRejectsChangedPlan(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	const (
		source   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		previous = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		result   = "cccccccccccccccccccccccccccccccccccccccc"
	)

	promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/plan-reject", SourceSHA: source,
		TargetRef: "main", Actor: "SHA256:operator",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Plan the promotion.
	now = now.Add(time.Second)
	if _, err := s.PlanPromotion(ctx, promotion.ID, previous, result); err != nil {
		t.Fatal(err)
	}

	// Same plan again: idempotent.
	if _, err := s.PlanPromotion(ctx, promotion.ID, previous, result); err != nil {
		t.Fatalf("idempotent plan = %v", err)
	}

	// Different plan: rejected.
	if _, err := s.PlanPromotion(ctx, promotion.ID, previous, "dddddddddddddddddddddddddddddddddddddddd"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("changed plan = %v, want ErrInvalidState", err)
	}

	// Invalid fields.
	if _, err := s.PlanPromotion(ctx, "", previous, result); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty ID = %v, want ErrInvalid", err)
	}
	if _, err := s.PlanPromotion(ctx, promotion.ID, "", "not-a-sha"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid result = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// finishPromotionTx — exercised through FinishPromotion with interrupted status
// ---------------------------------------------------------------------------

func TestFinishPromotionInterruptedStatus(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/interrupt", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "main", PreviousSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ResultSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "SHA256:operator",
	})
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Second)
	finished, err := s.FinishPromotion(ctx, promotion.ID, model.PromotionInterrupted, "", "oberth restarted")
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != model.PromotionInterrupted || finished.Error != "oberth restarted" {
		t.Fatalf("interrupted promotion = %#v", finished)
	}

	// Already terminal: rejected.
	if _, err := s.FinishPromotion(ctx, promotion.ID, model.PromotionInterrupted, "", "retry"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("double finish = %v, want ErrInvalidState", err)
	}
}

// ---------------------------------------------------------------------------
// FinishPromotion validation
// ---------------------------------------------------------------------------

func TestFinishPromotionValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Invalid status (not failed or interrupted).
	if _, err := s.FinishPromotion(ctx, "some-id", model.PromotionPassed, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("FinishPromotion(passed) = %v, want ErrInvalid", err)
	}
	// Empty ID.
	if _, err := s.FinishPromotion(ctx, "", model.PromotionFailed, "", "fail"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("FinishPromotion(empty) = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// AuditHeadHint
// ---------------------------------------------------------------------------

func TestAuditHeadHint(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Empty chain: returns genesis head with ID 0.
	hint, err := s.AuditHeadHint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hint.ID != 0 || len(hint.SHA256) != sha256.Size {
		t.Fatalf("empty AuditHeadHint = %#v", hint)
	}

	// After one action.
	if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test.hint", ResourceType: "test", ResourceID: "1",
	}); err != nil {
		t.Fatal(err)
	}
	hint, err = s.AuditHeadHint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hint.ID != 1 || len(hint.SHA256) != sha256.Size {
		t.Fatalf("AuditHeadHint after action = %#v", hint)
	}
}

// ---------------------------------------------------------------------------
// VerifyAuditMutationStateUnanchored
// ---------------------------------------------------------------------------

func TestVerifyAuditMutationStateUnanchored(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test.unanchored", ResourceType: "test", ResourceID: "1",
	}); err != nil {
		t.Fatal(err)
	}

	var callbackHead model.AuditHead
	head, _, err := s.VerifyAuditMutationStateUnanchored(ctx, nil, nil, func(h model.AuditHead, a model.AuditAnchor) error {
		callbackHead = h
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if head.ID != 1 || callbackHead.ID != 1 {
		t.Fatalf("unanchored head = %d, callback head = %d", head.ID, callbackHead.ID)
	}
}

// ---------------------------------------------------------------------------
// VerifyAuditWitnessIntents
// ---------------------------------------------------------------------------

func TestVerifyAuditWitnessIntents(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	action, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test.witness", ResourceType: "test", ResourceID: "1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Valid intent.
	intents := []model.AuditWitnessIntent{{
		Sequence:    1,
		AuditID:     action.ID,
		AuditSHA256: action.SHA256,
	}}
	if err := s.VerifyAuditWitnessIntents(ctx, intents); err != nil {
		t.Fatalf("valid intents = %v", err)
	}

	// Invalid intent (wrong hash).
	badIntents := []model.AuditWitnessIntent{{
		Sequence:    1,
		AuditID:     action.ID,
		AuditSHA256: make([]byte, sha256.Size),
	}}
	if err := s.VerifyAuditWitnessIntents(ctx, badIntents); err == nil {
		t.Fatal("bad intents should fail")
	}

	// Empty intents: pass.
	if err := s.VerifyAuditWitnessIntents(ctx, nil); err != nil {
		t.Fatalf("empty intents = %v", err)
	}
}

// ---------------------------------------------------------------------------
// DistinctBranchRefsForSHA
// ---------------------------------------------------------------------------

func TestDistinctBranchRefsForSHA(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Same SHA pushed to two branches.
	if _, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/one", sha)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/two", sha)); err != nil {
		t.Fatal(err)
	}

	refs, err := s.DistinctBranchRefsForSHA(ctx, repo.ID, sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("distinct refs = %v, want 2", refs)
	}

	// Validation: invalid SHA.
	if _, err := s.DistinctBranchRefsForSHA(ctx, repo.ID, "short"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid SHA = %v, want ErrInvalid", err)
	}
	// Validation: invalid repo.
	if _, err := s.DistinctBranchRefsForSHA(ctx, 0, sha); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid repo = %v, want ErrInvalid", err)
	}

	// Nonexistent SHA: empty result.
	refs, err = s.DistinctBranchRefsForSHA(ctx, repo.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("nonexistent SHA refs = %v", refs)
	}
}

// ---------------------------------------------------------------------------
// CompleteSupersededRunCancellationWithoutJob
// ---------------------------------------------------------------------------

func TestCompleteSupersededRunCancellationWithoutJob(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	// Validation.
	if err := s.CompleteSupersededRunCancellationWithoutJob(ctx, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty ID = %v, want ErrInvalid", err)
	}

	// Enqueue, claim, supersede.
	first, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/no-job-cancel", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/no-job-cancel", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")); err != nil {
		t.Fatal(err)
	}

	// The first run is now interrupted with no job name.
	// CompleteSupersededRunCancellationWithoutJob should close it.
	if err := s.CompleteSupersededRunCancellationWithoutJob(ctx, first.ID); err != nil {
		t.Fatal(err)
	}

	// Nonexistent: no error, just a no-op.
	if err := s.CompleteSupersededRunCancellationWithoutJob(ctx, "nonexistent"); err != nil {
		t.Fatalf("nonexistent = %v", err)
	}
}

// ---------------------------------------------------------------------------
// SetRepositoryDefaultBranch
// ---------------------------------------------------------------------------

func TestSetRepositoryDefaultBranch(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	updated, err := s.SetRepositoryDefaultBranch(ctx, repo.ID, "develop")
	if err != nil {
		t.Fatal(err)
	}
	if updated.DefaultBranch != "develop" {
		t.Fatalf("updated default branch = %q, want develop", updated.DefaultBranch)
	}

	// Validation.
	if _, err := s.SetRepositoryDefaultBranch(ctx, 0, "main"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero ID = %v, want ErrInvalid", err)
	}
	if _, err := s.SetRepositoryDefaultBranch(ctx, repo.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty branch = %v, want ErrInvalid", err)
	}
	if _, err := s.SetRepositoryDefaultBranch(ctx, 99999, "main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing repo = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// RecoverOwnerState
// ---------------------------------------------------------------------------

func TestRecoverOwnerState(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// RecoverOwnerState on an empty database is a no-op.
	if err := s.RecoverOwnerState(ctx); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// AuthenticatedUplinkByDigest
// ---------------------------------------------------------------------------

func TestAuthenticatedUplinkByDigest(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	digest := make([]byte, 32)
	digest[0] = 0xDD
	cred, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "auth@host", Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertUplink(ctx, model.UplinkSpec{
		Fingerprint: "SHA256:auth-test", Identity: "auth@host",
		TokenCredentialID: cred.ID, AuthActor: "token:" + cred.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTokenCredential(ctx, cred.ID); err != nil {
		t.Fatal(err)
	}

	// Successful lookup.
	authed, err := s.AuthenticatedUplinkByDigest(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if authed.Identity != "auth@host" || authed.Fingerprint != "SHA256:auth-test" {
		t.Fatalf("authenticated uplink = %#v", authed)
	}
	if authed.TokenCredential.Name != "auth@host" || authed.TokenCredential.ActivatedAt == nil {
		t.Fatalf("token credential = %#v", authed.TokenCredential)
	}

	// Wrong digest: not found.
	if _, err := s.AuthenticatedUplinkByDigest(ctx, make([]byte, 32)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong digest = %v, want ErrNotFound", err)
	}
	// Short digest: not found.
	if _, err := s.AuthenticatedUplinkByDigest(ctx, make([]byte, 16)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("short digest = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// TouchTokenCredential
// ---------------------------------------------------------------------------

func TestTouchTokenCredential(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	digest := make([]byte, 32)
	digest[0] = 0xEE
	cred, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "touch@host", Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertUplink(ctx, model.UplinkSpec{
		Fingerprint: "SHA256:touch-test", Identity: "touch@host",
		TokenCredentialID: cred.ID, AuthActor: "token:" + cred.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTokenCredential(ctx, cred.ID); err != nil {
		t.Fatal(err)
	}

	// Touch updates last_used_at.
	now = now.Add(time.Minute)
	if err := s.TouchTokenCredential(ctx, cred.ID); err != nil {
		t.Fatal(err)
	}
	readback, err := s.TokenCredentialByDigest(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if readback.LastUsedAt == nil {
		t.Fatal("last_used_at should be set after touch")
	}

	// Touch a revoked credential: fails.
	if err := s.RevokeTokenCredential(ctx, cred.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchTokenCredential(ctx, cred.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("touch revoked = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// RevokeTokenCredential
// ---------------------------------------------------------------------------

func TestRevokeTokenCredential(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Revoke a pending (not activated) credential.
	digest1 := make([]byte, 32)
	digest1[0] = 0xF1
	pending, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "pending@host", Digest: digest1})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeTokenCredential(ctx, pending.ID); err != nil {
		t.Fatal(err)
	}
	readback, err := s.TokenCredentialByDigest(ctx, digest1)
	if err != nil {
		t.Fatal(err)
	}
	if readback.RevokedAt == nil {
		t.Fatal("pending credential should be revoked")
	}

	// Revoke an activated credential.
	digest2 := make([]byte, 32)
	digest2[0] = 0xF2
	active, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "active@host", Digest: digest2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertUplink(ctx, model.UplinkSpec{
		Fingerprint: "SHA256:revoke-test", Identity: "active@host",
		TokenCredentialID: active.ID, AuthActor: "token:" + active.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTokenCredential(ctx, active.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeTokenCredential(ctx, active.ID); err != nil {
		t.Fatal(err)
	}
	readback2, err := s.TokenCredentialByDigest(ctx, digest2)
	if err != nil {
		t.Fatal(err)
	}
	if readback2.RevokedAt == nil {
		t.Fatal("active credential should be revoked")
	}

	// Revoke nonexistent: fails.
	if err := s.RevokeTokenCredential(ctx, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke nonexistent = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// TrustedPlan and TrustedApply lookup — ErrNotFound on missing
// ---------------------------------------------------------------------------

func TestTrustedPlanLookup(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.TrustedPlan(ctx, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TrustedPlan(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.TrustedPlan(ctx, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("TrustedPlan(empty) = %v, want ErrInvalid", err)
	}

	// Insert a test trusted plan row to exercise the success scan path.
	repo := createRepo(t, s)
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digest := strings.Repeat("ab", 32) // 64-char hex
	greenRun, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/tp", sha))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	planRun, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/tp-plan", sha))
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := s.Upstream(ctx, repo.UpstreamID)
	if err != nil {
		t.Fatal(err)
	}
	createdNano := unixNano(now)
	expiresNano := unixNano(now.Add(time.Hour))
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO trusted_plans(id, repo_id, upstream_id, source_ref, source_sha, target_ref,
	base_sha, result_sha, green_run_id, plan_run_id, backend_identity, backend_key,
	tool_digest, lock_digest, config_digest, actor, status, created_at, expires_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'authorized', ?, ?, ?)`,
		"tp-id-1", repo.ID, upstream.ID, "feature/tp", sha, "main",
		sha, sha, greenRun.ID, planRun.ID, "backend@host", "backend-key-1",
		digest, digest, digest, "agent@host", createdNano, expiresNano, createdNano,
	); err != nil {
		t.Fatal(err)
	}

	plan, err := s.TrustedPlan(ctx, "tp-id-1")
	if err != nil {
		t.Fatal(err)
	}
	if plan.ID != "tp-id-1" || plan.RepoID != repo.ID || plan.SourceRef != "feature/tp" ||
		plan.CreatedAt.IsZero() || plan.ExpiresAt.IsZero() {
		t.Fatalf("TrustedPlan lookup = %#v", plan)
	}
}

func TestTrustedApplyLookup(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.TrustedApply(ctx, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TrustedApply(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.TrustedApply(ctx, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("TrustedApply(empty) = %v, want ErrInvalid", err)
	}

	// Insert test data for TrustedApply to exercise the success scan path.
	repo := createRepo(t, s)
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digest := strings.Repeat("cd", 32) // 64-char hex

	// Create supporting records.
	greenRun, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/ta-green", sha))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	planRun, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/ta-plan", sha))
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := s.Upstream(ctx, repo.UpstreamID)
	if err != nil {
		t.Fatal(err)
	}
	createdNano := unixNano(now)
	expiresNano := unixNano(now.Add(time.Hour))

	// Insert trusted_plan.
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO trusted_plans(id, repo_id, upstream_id, source_ref, source_sha, target_ref,
	base_sha, result_sha, green_run_id, plan_run_id, backend_identity, backend_key,
	tool_digest, lock_digest, config_digest, actor, status, created_at, expires_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'authorized', ?, ?, ?)`,
		"ta-plan-1", repo.ID, upstream.ID, "feature/ta", sha, "main",
		sha, sha, greenRun.ID, planRun.ID, "backend@host", "backend-key-2",
		digest, digest, digest, "agent@host", createdNano, expiresNano, createdNano,
	); err != nil {
		t.Fatal(err)
	}

	// Create a promotion.
	now = now.Add(time.Second)
	promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/ta", SourceSHA: sha,
		TargetRef: "main", PreviousSHA: sha, ResultSHA: sha, Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create the apply run.
	now = now.Add(time.Second)
	applyRun, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/ta-apply", sha))
	if err != nil {
		t.Fatal(err)
	}

	// Insert trusted_apply.
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO trusted_applies(id, plan_id, promotion_id, repo_id, target_ref, sha, run_id,
	artifact_digest, backend_key, actor, status, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?)`,
		"ta-id-1", "ta-plan-1", promotion.ID, repo.ID, "main", sha, applyRun.ID,
		digest, "backend-key-2", "agent@host", createdNano, createdNano,
	); err != nil {
		t.Fatal(err)
	}

	apply, err := s.TrustedApply(ctx, "ta-id-1")
	if err != nil {
		t.Fatal(err)
	}
	if apply.ID != "ta-id-1" || apply.PlanID != "ta-plan-1" || apply.RepoID != repo.ID {
		t.Fatalf("TrustedApply lookup = %#v", apply)
	}
}

// ---------------------------------------------------------------------------
// EnqueueAdmittedPromotionRun
// ---------------------------------------------------------------------------

func TestEnqueueAdmittedPromotionRun(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	const (
		source   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		previous = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		result   = "cccccccccccccccccccccccccccccccccccccccc"
	)

	// Create a promotion, then plan it.
	promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/admitted", SourceSHA: source,
		TargetRef: "main", Actor: "agent@host",
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	planned, err := s.PlanPromotion(ctx, promotion.ID, previous, result)
	if err != nil {
		t.Fatal(err)
	}

	// Enqueue the admitted run.
	runSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/" + result[:12],
		SHA: result, Actor: "agent@host", Trigger: "promotion",
		TestedSHA: result, BaseSHA: previous,
	}
	now = now.Add(time.Second)
	enqueued, updatedPromo, err := s.EnqueueAdmittedPromotionRun(ctx, runSpec, planned.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updatedPromo.RunID != enqueued.ID || updatedPromo.Status != model.PromotionPending {
		t.Fatalf("admitted promotion = %#v / run = %#v", updatedPromo, enqueued.Run)
	}

	// Validation: empty promotion ID.
	if _, _, err := s.EnqueueAdmittedPromotionRun(ctx, runSpec, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty promotion ID = %v, want ErrInvalid", err)
	}

	// Validation: mismatched run spec (wrong trigger).
	badSpec := runSpec
	badSpec.Trigger = "push"
	if _, _, err := s.EnqueueAdmittedPromotionRun(ctx, badSpec, planned.ID); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong trigger = %v", err)
	}
}

// ---------------------------------------------------------------------------
// CompleteRunCancellation — full lifecycle and idempotency
// ---------------------------------------------------------------------------

func TestCompleteRunCancellationIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	// Create a run, claim it, set job name, supersede it.
	first, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/cancel-idempotent", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunJobName(ctx, first.ID, "job-cancel"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/cancel-idempotent", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")); err != nil {
		t.Fatal(err)
	}

	// Complete the cancellation.
	if err := s.CompleteRunCancellation(ctx, first.ID, "job-cancel"); err != nil {
		t.Fatal(err)
	}
	// Idempotent: completing again should succeed.
	if err := s.CompleteRunCancellation(ctx, first.ID, "job-cancel"); err != nil {
		t.Fatalf("idempotent completion = %v", err)
	}

	// Not found.
	if err := s.CompleteRunCancellation(ctx, "nonexistent", "job-cancel"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing cancellation = %v, want ErrNotFound", err)
	}

	// Validation.
	if err := s.CompleteRunCancellation(ctx, "", "job"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty run ID = %v, want ErrInvalid", err)
	}
	if err := s.CompleteRunCancellation(ctx, first.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty job name = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// SetRunJobName — terminal run rejection
// ---------------------------------------------------------------------------

func TestSetRunJobNameTerminalRejection(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	// Create, claim, finish.
	enqueued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/job-terminal", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{
		Status: model.RunPassed, Phase: "build",
	}); err != nil {
		t.Fatal(err)
	}

	// Setting job name on a terminal run: rejected.
	if _, err := s.SetRunJobName(ctx, enqueued.ID, "late-job"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("terminal job name = %v, want ErrInvalidState", err)
	}

	// Nonexistent run.
	if _, err := s.SetRunJobName(ctx, "nonexistent", "job"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run job name = %v, want ErrNotFound", err)
	}

	// Validation.
	if _, err := s.SetRunJobName(ctx, "", "job"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty run ID = %v, want ErrInvalid", err)
	}
	if _, err := s.SetRunJobName(ctx, enqueued.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty job name = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// validatePromotionAdmissionSpec — edge cases
// ---------------------------------------------------------------------------

func TestValidatePromotionAdmissionSpecEdgeCases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)

	// Missing actor.
	if _, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/val", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "main", Actor: "",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor = %v, want ErrInvalid", err)
	}
	// Invalid source SHA.
	if _, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/val", SourceSHA: "not-a-sha",
		TargetRef: "main", Actor: "SHA256:test",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid source SHA = %v, want ErrInvalid", err)
	}
	// PreviousSHA present without ResultSHA (must be wholly present or absent).
	if _, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/val", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "main", PreviousSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Actor: "SHA256:test",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("partial plan = %v, want ErrInvalid", err)
	}
	// Invalid PreviousSHA format.
	if _, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/val", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "main", PreviousSHA: "not-a-sha", ResultSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Actor: "SHA256:test",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid previous SHA = %v, want ErrInvalid", err)
	}
	// Invalid ResultSHA format.
	if _, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/val", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "main", PreviousSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ResultSHA: "not-a-sha",
		Actor: "SHA256:test",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid result SHA = %v, want ErrInvalid", err)
	}
	// Zero RepoID.
	if _, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: 0, SourceBranch: "feature/val", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "main", Actor: "SHA256:test",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero repo ID = %v, want ErrInvalid", err)
	}
	// Empty branch.
	if _, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "main", Actor: "SHA256:test",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty branch = %v, want ErrInvalid", err)
	}
	// Empty target ref.
	if _, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/val", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "", Actor: "SHA256:test",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty target = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// PromotionByRun — edge cases
// ---------------------------------------------------------------------------

func TestPromotionByRunEdgeCases(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Empty run ID.
	if _, err := s.PromotionByRun(ctx, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty run ID = %v, want ErrInvalid", err)
	}
	// Nonexistent run.
	if _, err := s.PromotionByRun(ctx, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// validateRunSpec — edge cases
// ---------------------------------------------------------------------------

func TestValidateRunSpecEdgeCases(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	// Missing ref.
	if _, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Actor: "SHA256:test", Trigger: "push",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty ref = %v, want ErrInvalid", err)
	}
	// Invalid SHA.
	if _, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: "bad",
		Actor: "SHA256:test", Trigger: "push",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad SHA = %v, want ErrInvalid", err)
	}
	// Invalid TestedSHA.
	if _, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Actor: "SHA256:test", Trigger: "push", TestedSHA: "bad",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad tested SHA = %v, want ErrInvalid", err)
	}
	// Invalid BaseSHA.
	if _, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Actor: "SHA256:test", Trigger: "push", BaseSHA: "bad",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad base SHA = %v, want ErrInvalid", err)
	}
	// Missing actor.
	if _, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Actor: "", Trigger: "push",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor = %v, want ErrInvalid", err)
	}
	// Missing trigger.
	if _, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Actor: "SHA256:test", Trigger: "",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty trigger = %v, want ErrInvalid", err)
	}
	// Zero repo ID.
	if _, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: 0, RefKind: model.RefBranch, Ref: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Actor: "SHA256:test", Trigger: "push",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero repo = %v, want ErrInvalid", err)
	}
	// Invalid RefKind.
	if _, err := s.EnqueueRun(ctx, model.RunSpec{
		RepoID: repo.ID, RefKind: "invalid", Ref: "main", SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Actor: "SHA256:test", Trigger: "push",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid ref kind = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// FinishRun — edge cases
// ---------------------------------------------------------------------------

func TestFinishRunEdgeCases(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Empty ID.
	if _, err := s.FinishRun(ctx, "", model.RunResult{Status: model.RunPassed}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty run ID = %v, want ErrInvalid", err)
	}
	// Non-terminal status.
	if _, err := s.FinishRun(ctx, "some-id", model.RunResult{Status: model.RunRunning}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-terminal = %v, want ErrInvalid", err)
	}
	// Invalid TestedSHA.
	if _, err := s.FinishRun(ctx, "some-id", model.RunResult{Status: model.RunFailed, TestedSHA: "bad"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad tested SHA = %v, want ErrInvalid", err)
	}
	// Nonexistent run.
	if _, err := s.FinishRun(ctx, "nonexistent", model.RunResult{Status: model.RunFailed, Error: "fail"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// CreateUpstream/CreateRepository validation
// ---------------------------------------------------------------------------

func TestCreateUpstreamValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "", Kind: "ssh", BaseURL: "ssh://x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty name = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "x", Kind: "", BaseURL: "ssh://x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty kind = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "x", Kind: "ssh", BaseURL: ""}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty base URL = %v, want ErrInvalid", err)
	}
}

func TestCreateRepositoryValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "", UpstreamID: 1, DefaultBranch: "main"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty name = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "x", UpstreamID: 0, DefaultBranch: "main"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero upstream = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "x", UpstreamID: 1, DefaultBranch: ""}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty branch = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// Close idempotency
// ---------------------------------------------------------------------------

func TestCloseIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	// Closing twice: no error.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Promotion lookup edge case
// ---------------------------------------------------------------------------

func TestPromotionLookupNotFound(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.Promotion(ctx, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Promotion(missing) = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// UpsertUplink validation
// ---------------------------------------------------------------------------

func TestUpsertUplinkValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.UpsertUplink(ctx, model.UplinkSpec{
		Fingerprint: "", Identity: "x", TokenCredentialID: "x", AuthActor: "x",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty fingerprint = %v, want ErrInvalid", err)
	}
	if _, err := s.UpsertUplink(ctx, model.UplinkSpec{
		Fingerprint: "x", Identity: "", TokenCredentialID: "x", AuthActor: "x",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty identity = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// UplinkByFingerprint not found
// ---------------------------------------------------------------------------

func TestUplinkByFingerprintNotFound(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.UplinkByFingerprint(ctx, "SHA256:nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UplinkByFingerprint(missing) = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// CreateTokenCredential validation
// ---------------------------------------------------------------------------

func TestCreateTokenCredentialValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "", Digest: make([]byte, 32)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty name = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "x", Digest: make([]byte, 16)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short digest = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// Open validation
// ---------------------------------------------------------------------------

func TestOpenValidation(t *testing.T) {
	if _, err := Open(nil, "/path", Options{}); !errors.Is(err, ErrInvalid) { //nolint:staticcheck
		t.Fatalf("nil context = %v, want ErrInvalid", err)
	}
	if _, err := Open(context.Background(), "", Options{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty path = %v, want ErrInvalid", err)
	}
	if _, err := Open(context.Background(), "   ", Options{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("whitespace path = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// AppendAuditAction validation
// ---------------------------------------------------------------------------

func TestAppendAuditActionValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Empty actor.
	if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "", Action: "test", ResourceType: "test",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor = %v, want ErrInvalid", err)
	}
	// Invalid JSON details.
	if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent", Action: "test", ResourceType: "test", Details: "not-json",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid JSON = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// ListUpstreams — covers the upstream listing path
// ---------------------------------------------------------------------------

func TestListUpstreams(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Empty initially.
	upstreams, err := s.ListUpstreams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(upstreams) != 0 {
		t.Fatalf("expected empty, got %d", len(upstreams))
	}

	// Create two.
	if _, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "alpha", Kind: "ssh", BaseURL: "ssh://alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "beta", Kind: "ssh", BaseURL: "ssh://beta"}); err != nil {
		t.Fatal(err)
	}
	upstreams, err = s.ListUpstreams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(upstreams) != 2 {
		t.Fatalf("expected 2, got %d", len(upstreams))
	}
}

// ---------------------------------------------------------------------------
// EnqueueRun with tag ref (covers RefTag code path in enqueueRunTx)
// ---------------------------------------------------------------------------

func TestEnqueueRunTagRefSkipsSupersession(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	spec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefTag, Ref: "v1.0.0",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "SHA256:test", Trigger: "tag",
		Release: true,
	}
	result, err := s.EnqueueRun(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Release || result.RefKind != model.RefTag {
		t.Fatalf("tag run = %#v", result.Run)
	}
	if len(result.Cancellations) != 0 {
		t.Fatal("tag runs should not supersede")
	}
}

// ---------------------------------------------------------------------------
// RecordAuditAnchor validation
// ---------------------------------------------------------------------------

func TestRecordAuditAnchorValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Invalid AuditID.
	if _, err := s.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: -1, AuditSHA256: make([]byte, sha256.Size),
		TSAURL: "https://tsa.example.com", Receipt: []byte("data"),
		AnchoredAt: now,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative audit ID = %v, want ErrInvalid", err)
	}
	// Missing TSAURL.
	if _, err := s.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: 0, AuditSHA256: make([]byte, sha256.Size),
		TSAURL: "", Receipt: []byte("data"),
		AnchoredAt: now,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty TSA URL = %v, want ErrInvalid", err)
	}
	// Empty receipt.
	if _, err := s.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: 0, AuditSHA256: make([]byte, sha256.Size),
		TSAURL: "https://tsa.example.com", Receipt: nil,
		AnchoredAt: now,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil receipt = %v, want ErrInvalid", err)
	}
	// Wrong SHA size.
	if _, err := s.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: 0, AuditSHA256: make([]byte, 16),
		TSAURL: "https://tsa.example.com", Receipt: []byte("data"),
		AnchoredAt: now,
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong SHA size = %v, want ErrInvalid", err)
	}
	// Zero time.
	if _, err := s.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: 0, AuditSHA256: make([]byte, sha256.Size),
		TSAURL: "https://tsa.example.com", Receipt: []byte("data"),
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero time = %v, want ErrInvalid", err)
	}

	// Mismatched SHA.
	action, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test.anchor.val", ResourceType: "test", ResourceID: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: action.ID, AuditSHA256: make([]byte, sha256.Size), // wrong hash
		TSAURL: "https://tsa.example.com", Receipt: []byte("data"),
		AnchoredAt: now,
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("mismatched SHA = %v, want ErrInvalidState", err)
	}

	// Genesis anchor (AuditID=0) with correct SHA (all zeros).
	anchor, err := s.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: 0, AuditSHA256: make([]byte, sha256.Size),
		TSAURL: "https://tsa.example.com", Receipt: []byte("genesis-receipt"),
		AnchoredAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if anchor.TSAURL != "https://tsa.example.com" || anchor.AuditID != 0 {
		t.Fatalf("genesis anchor = %#v", anchor)
	}
}

// ---------------------------------------------------------------------------
// VerifyAuditWitnesses full path
// ---------------------------------------------------------------------------

func TestVerifyAuditWitnesses(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	action, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test.witness.full", ResourceType: "test", ResourceID: "1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Record an anchor.
	head, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordAuditAnchor(ctx, model.AuditAnchorSpec{
		AuditID: head.ID, AuditSHA256: head.SHA256,
		TSAURL: "https://tsa.example.com", Receipt: []byte("receipt"),
		AnchoredAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Verify with a matching witness.
	witnesses := []model.AuditWitness{{
		UUID: "witness-1", LogIndex: 0, IntegratedAt: now,
		AuditID: action.ID, AuditSHA256: action.SHA256,
	}}
	if err := s.VerifyAuditWitnesses(ctx, witnesses); err != nil {
		t.Fatalf("valid witnesses = %v", err)
	}

	// Verify with wrong hash.
	badWitnesses := []model.AuditWitness{{
		UUID: "witness-bad", LogIndex: 0, IntegratedAt: now,
		AuditID: action.ID, AuditSHA256: make([]byte, sha256.Size),
	}}
	if err := s.VerifyAuditWitnesses(ctx, badWitnesses); err == nil {
		t.Fatal("bad witness should fail")
	}

	// Empty witnesses: should fail because anchor has no witness.
	if err := s.VerifyAuditWitnesses(ctx, nil); err == nil {
		t.Fatal("empty witnesses with anchor should fail")
	}
}

// ---------------------------------------------------------------------------
// VerifyAuditMutationState with callback error
// ---------------------------------------------------------------------------

func TestVerifyAuditMutationStateCallbackError(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: "agent@host", Action: "test.callback", ResourceType: "test", ResourceID: "1",
	}); err != nil {
		t.Fatal(err)
	}

	// Nil callback: ErrInvalid.
	if _, _, err := s.VerifyAuditMutationState(ctx, nil, nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil callback = %v, want ErrInvalid", err)
	}

	// Callback that returns an error.
	callbackErr := errors.New("callback failed")
	_, _, err := s.VerifyAuditMutationStateUnanchored(ctx, nil, nil, func(h model.AuditHead, a model.AuditAnchor) error {
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("callback error = %v, want %v", err, callbackErr)
	}
}

// ---------------------------------------------------------------------------
// EnqueuePromotionRun validation edge cases
// ---------------------------------------------------------------------------

func TestEnqueuePromotionRunValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	const (
		base   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		merged = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)

	// Valid run spec but mismatched promotion spec (wrong actor).
	runSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/bbbb",
		SHA: merged, Actor: "agent@host", Trigger: "promotion",
		TestedSHA: merged, BaseSHA: base,
	}
	promoSpec := model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/one", SourceSHA: merged,
		TargetRef: "main", PreviousSHA: base, ResultSHA: merged,
		Actor: "different@host",
	}
	if _, _, err := s.EnqueuePromotionRun(ctx, runSpec, promoSpec); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched actor = %v, want ErrInvalid", err)
	}

	// Non-promotion trigger.
	badTrigger := runSpec
	badTrigger.Trigger = "push"
	if _, _, err := s.EnqueuePromotionRun(ctx, badTrigger, promoSpec); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong trigger = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// validatePromotionSpec edge cases (validates ResultSHA)
// ---------------------------------------------------------------------------

func TestValidatePromotionSpecResultSHA(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	// EnqueuePromotionRun goes through validatePromotionSpec, which validates ResultSHA.
	// Invalid ResultSHA.
	runSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/short",
		SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host", Trigger: "promotion",
		TestedSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}
	badPromoSpec := model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/bad", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "main", PreviousSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ResultSHA: "short", Actor: "agent@host",
	}
	if _, _, err := s.EnqueuePromotionRun(ctx, runSpec, badPromoSpec); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid result SHA = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// RecordReceiveEvent validation + matchReceiveRun + validateReceiveEventSpec
// ---------------------------------------------------------------------------

func TestRecordReceiveEventValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	// Empty ID.
	if _, err := s.RecordReceiveEvent(ctx, model.ReceiveEventSpec{
		ID: "", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefBranch, Ref: "main", Outcome: "deleted",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty ID = %v, want ErrInvalid", err)
	}
	// Missing actor.
	if _, err := s.RecordReceiveEvent(ctx, model.ReceiveEventSpec{
		ID: "event-1", Actor: "", RepoID: repo.ID,
		RefKind: model.RefBranch, Ref: "main", Outcome: "deleted",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor = %v, want ErrInvalid", err)
	}
	// Invalid outcome.
	if _, err := s.RecordReceiveEvent(ctx, model.ReceiveEventSpec{
		ID: "event-2", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefBranch, Ref: "main", Outcome: "unknown",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid outcome = %v, want ErrInvalid", err)
	}
	// Invalid OldSHA.
	if _, err := s.RecordReceiveEvent(ctx, model.ReceiveEventSpec{
		ID: "event-3", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefBranch, Ref: "main", Outcome: "deleted",
		OldSHA: "bad",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad old SHA = %v, want ErrInvalid", err)
	}
	// release_rejected without object SHAs.
	if _, err := s.RecordReceiveEvent(ctx, model.ReceiveEventSpec{
		ID: "event-4", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefTag, Ref: "v1.0.0", Outcome: "release_rejected",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("release without SHAs = %v, want ErrInvalid", err)
	}

	// Valid deletion.
	dup, err := s.RecordReceiveEvent(ctx, model.ReceiveEventSpec{
		ID: "event-ok", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefBranch, Ref: "feature/deleted", Outcome: "deleted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dup {
		t.Fatal("first record should not be duplicate")
	}

	// Replay: duplicate.
	dup, err = s.RecordReceiveEvent(ctx, model.ReceiveEventSpec{
		ID: "event-ok", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefBranch, Ref: "feature/deleted", Outcome: "deleted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !dup {
		t.Fatal("second record should be duplicate")
	}

	// Valid release_rejected with SHAs.
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := s.RecordReceiveEvent(ctx, model.ReceiveEventSpec{
		ID: "event-rr", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefTag, Ref: "v1.0.0", Outcome: "release_rejected",
		ObjectSHA: sha, CommitSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEnqueueReceiveEventValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Mismatched actor between event and run.
	event := model.ReceiveEventSpec{
		ID: "enq-1", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefBranch, Ref: "feature/x", ObjectSHA: sha, CommitSHA: sha, Outcome: "queued",
	}
	runSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/x",
		SHA: sha, TestedSHA: sha, Actor: "different@host", Trigger: "branch",
	}
	if _, err := s.EnqueueReceiveEvent(ctx, event, runSpec); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched actor = %v, want ErrInvalid", err)
	}

	// Wrong outcome for queued.
	badEvent := model.ReceiveEventSpec{
		ID: "enq-2", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefBranch, Ref: "feature/x", Outcome: "deleted",
	}
	if _, err := s.EnqueueReceiveEvent(ctx, badEvent, model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/x",
		SHA: sha, Actor: "agent@host", Trigger: "branch",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-queued outcome = %v, want ErrInvalid", err)
	}

	// Tag event with non-release run.
	tagEvent := model.ReceiveEventSpec{
		ID: "enq-3", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefTag, Ref: "v1.0.0", ObjectSHA: sha, CommitSHA: sha, Outcome: "queued",
	}
	nonReleaseRun := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefTag, Ref: "v1.0.0",
		SHA: sha, TestedSHA: sha, Actor: "agent@host", Trigger: "tag",
		Release: false,
	}
	if _, err := s.EnqueueReceiveEvent(ctx, tagEvent, nonReleaseRun); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-release tag run = %v, want ErrInvalid", err)
	}

	// Valid enqueue.
	validEvent := model.ReceiveEventSpec{
		ID: "enq-valid", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefBranch, Ref: "feature/enq", ObjectSHA: sha, CommitSHA: sha, Outcome: "queued",
	}
	validRun := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/enq",
		SHA: sha, TestedSHA: sha, Actor: "agent@host", Trigger: "branch",
	}
	result, err := s.EnqueueReceiveEvent(ctx, validEvent, validRun)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID == "" || result.Duplicate {
		t.Fatalf("enqueue result = %#v", result)
	}

	// Replay: returns duplicate.
	replay, err := s.EnqueueReceiveEvent(ctx, validEvent, validRun)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Duplicate || replay.ID != result.ID {
		t.Fatalf("replay result = %#v", replay)
	}
}

// ---------------------------------------------------------------------------
// Issue deletion and lock release edge cases
// ---------------------------------------------------------------------------

func TestDeleteManualIssueValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Missing actor.
	if err := s.DeleteManualIssue(ctx, "", 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor = %v, want ErrInvalid", err)
	}
	// Nonexistent issue.
	if err := s.DeleteManualIssue(ctx, "agent@host", 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing issue = %v, want ErrNotFound", err)
	}
}

func TestReleaseIssueLockEdgeCases(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	issue, err := s.CreateManualIssue(ctx, "agent@host", model.ManualIssueSpec{Title: "lock-release"})
	if err != nil {
		t.Fatal(err)
	}

	// Release without lock: ErrLockNotOwned.
	if err := s.ReleaseIssueLock(ctx, issue.ID, "SHA256:nobody"); !errors.Is(err, ErrLockNotOwned) {
		t.Fatalf("release without lock = %v, want ErrLockNotOwned", err)
	}

	// Acquire, then release by wrong owner: ErrLockNotOwned.
	if _, err := s.AcquireIssueLock(ctx, issue.ID, "SHA256:alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseIssueLock(ctx, issue.ID, "SHA256:bob"); !errors.Is(err, ErrLockNotOwned) {
		t.Fatalf("wrong owner release = %v, want ErrLockNotOwned", err)
	}

	// Correct owner release: success.
	if err := s.ReleaseIssueLock(ctx, issue.ID, "SHA256:alice"); err != nil {
		t.Fatalf("owner release = %v", err)
	}

	// Nonexistent issue.
	if err := s.ReleaseIssueLock(ctx, 99999, "SHA256:alice"); !errors.Is(err, ErrLockNotOwned) {
		t.Fatalf("nonexistent issue = %v, want ErrLockNotOwned", err)
	}
}

// ---------------------------------------------------------------------------
// UpsertCIIssue validation
// ---------------------------------------------------------------------------

func TestUpsertCIIssueValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	// Empty actor.
	if _, err := s.UpsertCIIssue(ctx, "", repo.ID, "main", "title", "body"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor = %v, want ErrInvalid", err)
	}
	// Zero repo ID.
	if _, err := s.UpsertCIIssue(ctx, "agent@host", 0, "main", "title", "body"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero repo = %v, want ErrInvalid", err)
	}
	// Empty branch.
	if _, err := s.UpsertCIIssue(ctx, "agent@host", repo.ID, "", "title", "body"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty branch = %v, want ErrInvalid", err)
	}
	// Empty title.
	if _, err := s.UpsertCIIssue(ctx, "agent@host", repo.ID, "main", "", "body"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty title = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// OpenCIIssue not-found path
// ---------------------------------------------------------------------------

func TestOpenCIIssueNotFound(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	if _, err := s.OpenCIIssue(ctx, repo.ID, "feature/nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenCIIssue(missing) = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// CloseCIIssue validation
// ---------------------------------------------------------------------------

func TestCloseCIIssueValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Empty actor.
	if _, err := s.CloseCIIssue(ctx, "", 1, "resolved"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// FinishRun with promotion trigger → failing run terminates promotion
// ---------------------------------------------------------------------------

func TestFinishRunPromotionTriggerFailed(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	const (
		base   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		merged = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		ref    = "promotion/main/bbbbbbbbbbbb"
	)
	runSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: merged,
		Actor: "agent@host", Trigger: "promotion", TestedSHA: merged, BaseSHA: base,
	}
	promotionSpec := model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/prom-fail", SourceSHA: merged,
		TargetRef: "main", PreviousSHA: base, ResultSHA: merged, Actor: "agent@host",
	}
	enqueued, promotion, err := s.EnqueuePromotionRun(ctx, runSpec, promotionSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}

	// Passing a promotion run is rejected (must go through publication).
	if _, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{
		Status: model.RunPassed, Phase: "build",
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("promotion passed = %v, want ErrInvalidState", err)
	}

	// Failing the promotion run terminates the promotion.
	now = now.Add(time.Minute)
	finished, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{
		Status: model.RunFailed, Phase: "test", Error: "tests failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != model.RunFailed {
		t.Fatalf("finished status = %s", finished.Status)
	}

	// The promotion should be failed.
	prom, err := s.Promotion(ctx, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prom.Status != model.PromotionFailed {
		t.Fatalf("promotion status = %s, want failed", prom.Status)
	}
}

// ---------------------------------------------------------------------------
// FinishRun with interrupted status on promotion trigger
// ---------------------------------------------------------------------------

func TestFinishRunPromotionTriggerInterrupted(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	const (
		base   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		merged = "cccccccccccccccccccccccccccccccccccccccc"
		ref    = "promotion/main/cccccccccccc"
	)
	runSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: ref, SHA: merged,
		Actor: "agent@host", Trigger: "promotion", TestedSHA: merged, BaseSHA: base,
	}
	promotionSpec := model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/prom-int", SourceSHA: merged,
		TargetRef: "main", PreviousSHA: base, ResultSHA: merged, Actor: "agent@host",
	}
	enqueued, promotion, err := s.EnqueuePromotionRun(ctx, runSpec, promotionSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Minute)
	if _, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{
		Status: model.RunInterrupted, Error: "oberth restarted",
	}); err != nil {
		t.Fatal(err)
	}

	prom, err := s.Promotion(ctx, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prom.Status != model.PromotionInterrupted {
		t.Fatalf("promotion status = %s, want interrupted", prom.Status)
	}
}

// ---------------------------------------------------------------------------
// CreateManualIssue validation
// ---------------------------------------------------------------------------

func TestCreateManualIssueValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Empty actor.
	if _, err := s.CreateManualIssue(ctx, "", model.ManualIssueSpec{Title: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor = %v, want ErrInvalid", err)
	}
	// Empty title.
	if _, err := s.CreateManualIssue(ctx, "agent@host", model.ManualIssueSpec{Title: ""}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty title = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// AcquireIssueLock validation
// ---------------------------------------------------------------------------

func TestAcquireIssueLockValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Nonexistent issue: FK constraint error (not ErrNotFound).
	if _, err := s.AcquireIssueLock(ctx, 99999, "SHA256:alice"); err == nil {
		t.Fatal("nonexistent issue lock should fail")
	}
}

// ---------------------------------------------------------------------------
// RenewIssueLock validation
// ---------------------------------------------------------------------------

func TestRenewIssueLockNotOwned(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	issue, err := s.CreateManualIssue(ctx, "agent@host", model.ManualIssueSpec{Title: "renew-test"})
	if err != nil {
		t.Fatal(err)
	}
	// No lock: ErrLockNotOwned.
	if _, err := s.RenewIssueLock(ctx, issue.ID, "SHA256:alice"); !errors.Is(err, ErrLockNotOwned) {
		t.Fatalf("no lock renew = %v, want ErrLockNotOwned", err)
	}
	// Acquire by alice, renew by bob: ErrLockNotOwned.
	if _, err := s.AcquireIssueLock(ctx, issue.ID, "SHA256:alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewIssueLock(ctx, issue.ID, "SHA256:bob"); !errors.Is(err, ErrLockNotOwned) {
		t.Fatalf("wrong owner renew = %v, want ErrLockNotOwned", err)
	}
}

// ---------------------------------------------------------------------------
// issuePageLimit — limit > defaultPageSize
// ---------------------------------------------------------------------------

func TestIssuePageLimitClampsLargeLimit(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Create one issue so the query runs.
	if _, err := s.CreateManualIssue(ctx, "agent@host", model.ManualIssueSpec{Title: "big-limit"}); err != nil {
		t.Fatal(err)
	}
	// Limit > defaultPageSize (50) should be clamped.
	page, err := s.ListIssues(ctx, model.IssueListFilter{Limit: 999})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Issues) != 1 {
		t.Fatalf("page = %d", len(page.Issues))
	}
}

// ---------------------------------------------------------------------------
// Restart recovery with running promotion run
// ---------------------------------------------------------------------------

func TestRestartRecoveryWithPromotionRun(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth-prom-restart.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	const (
		base   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		merged = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	runSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/bbbbbbbbbbbb",
		SHA: merged, Actor: "agent@host", Trigger: "promotion", TestedSHA: merged, BaseSHA: base,
	}
	promotionSpec := model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/restart-prom", SourceSHA: merged,
		TargetRef: "main", PreviousSHA: base, ResultSHA: merged, Actor: "agent@host",
	}
	enqueued, promotion, err := s.EnqueuePromotionRun(ctx, runSpec, promotionSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart.
	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// The no-job promotion run should be interrupted.
	recovered, err := s.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunInterrupted {
		t.Fatalf("recovered promotion run status = %q, want interrupted", recovered.Status)
	}

	// The promotion should also be interrupted.
	prom, err := s.Promotion(ctx, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prom.Status != model.PromotionInterrupted {
		t.Fatalf("recovered promotion status = %q, want interrupted", prom.Status)
	}
}

// ---------------------------------------------------------------------------
// Restart recovery with pending promotion (no run)
// ---------------------------------------------------------------------------

func TestRestartRecoveryWithPendingPromotionNoRun(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth-pending-prom.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	// Create a promotion without a run (the run_id is empty in the AppendPromotion path).
	promotion, err := s.AppendPromotion(ctx, model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/no-run-prom", SourceSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		TargetRef: "main", PreviousSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ResultSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "SHA256:operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart.
	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// The no-run promotion should be interrupted.
	prom, err := s.Promotion(ctx, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prom.Status != model.PromotionInterrupted {
		t.Fatalf("recovered no-run promotion status = %q, want interrupted", prom.Status)
	}
}

// ---------------------------------------------------------------------------
// FinishRun with TestedSHA and BaseSHA overrides
// ---------------------------------------------------------------------------

func TestFinishRunWithSHAOverrides(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	enqueued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/overrides", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}

	overrideSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	baseSHA := "cccccccccccccccccccccccccccccccccccccccc"
	now = now.Add(time.Minute)
	finished, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{
		Status: model.RunFailed, Phase: "build", Error: "fail",
		TestedSHA: overrideSHA, BaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished.TestedSHA != overrideSHA || finished.BaseSHA != baseSHA {
		t.Fatalf("SHA overrides: tested=%q base=%q", finished.TestedSHA, finished.BaseSHA)
	}
}

// ---------------------------------------------------------------------------
// ResolveRun via non-hex selector (falls through to ref lookup)
// ---------------------------------------------------------------------------

func TestResolveRunNonHexSelector(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	if _, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/resolve-ref", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	// Non-hex selector: goes directly to ref lookup.
	resolved, err := s.ResolveRun(ctx, repo.ID, "feature/resolve-ref")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Ref != "feature/resolve-ref" {
		t.Fatalf("resolved ref = %q", resolved.Ref)
	}

	// Validation.
	if _, err := s.ResolveRun(ctx, 0, "main"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero repo = %v, want ErrInvalid", err)
	}
	if _, err := s.ResolveRun(ctx, repo.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty selector = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// SetRunJobName on queued run
// ---------------------------------------------------------------------------

func TestSetRunJobNameOnQueuedRun(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	enqueued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/queued-job", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	// Setting job name on a queued run should work.
	updated, err := s.SetRunJobName(ctx, enqueued.ID, "queued-job-name")
	if err != nil {
		t.Fatal(err)
	}
	if updated.JobName != "queued-job-name" || updated.Status != model.RunQueued {
		t.Fatalf("queued run after SetRunJobName = %#v", updated)
	}
}

// ---------------------------------------------------------------------------
// Restart recovery — running run with job name that is a promotion
// ---------------------------------------------------------------------------

func TestRestartRecoveryRunningPromotionWithJobName(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth-prom-job.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	const (
		base   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		merged = "dddddddddddddddddddddddddddddddddddddddd"
	)
	runSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/dddddddddddd",
		SHA: merged, Actor: "agent@host", Trigger: "promotion", TestedSHA: merged, BaseSHA: base,
	}
	promotionSpec := model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/prom-job-restart", SourceSHA: merged,
		TargetRef: "main", PreviousSHA: base, ResultSHA: merged, Actor: "agent@host",
	}
	enqueued, _, err := s.EnqueuePromotionRun(ctx, runSpec, promotionSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunJobName(ctx, enqueued.ID, "prom-job-restart"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: runs with known job names stay running for scheduler reconciliation.
	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	recovered, err := s.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != model.RunRunning {
		t.Fatalf("promotion run with job name should stay running, got %q", recovered.Status)
	}
	// A cancellation record should exist for owner_restart.
	cancellations, err := s.PendingRunCancellations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cancellations {
		if c.RunID == enqueued.ID && c.Reason == "owner_restart" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected owner_restart cancellation for promotion run with job name")
	}
}

// ---------------------------------------------------------------------------
// UpdateManualIssue validation
// ---------------------------------------------------------------------------

func TestUpdateManualIssueValidation(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	// Empty actor.
	title := "updated"
	if _, err := s.UpdateManualIssue(ctx, "", 1, model.IssuePatch{Title: &title}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty actor = %v, want ErrInvalid", err)
	}
	// Nonexistent issue.
	if _, err := s.UpdateManualIssue(ctx, "agent@host", 99999, model.IssuePatch{Title: &title}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing issue = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Run and LatestRunForRef/SHA not-found paths
// ---------------------------------------------------------------------------

func TestRunNotFoundPaths(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	if _, err := s.Run(ctx, "nonexistent-run"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Run(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.LatestRunForRef(ctx, repo.ID, "nonexistent-ref"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestRunForRef(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.LatestRunForSHA(ctx, repo.ID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestRunForSHA(missing) = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Restart recovery with a pending promotion whose run already failed
// ---------------------------------------------------------------------------

func TestRestartRecoveryPendingPromotionWithFailedRun(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth-prom-failed.db")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "forgejo", BaseURL: "https://codeberg.org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	const (
		base   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		merged = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	)
	runSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/eeeeeeeeeeee",
		SHA: merged, Actor: "agent@host", Trigger: "promotion", TestedSHA: merged, BaseSHA: base,
	}
	promotionSpec := model.PromotionSpec{
		RepoID: repo.ID, SourceBranch: "feature/prom-failed-restart", SourceSHA: merged,
		TargetRef: "main", PreviousSHA: base, ResultSHA: merged, Actor: "agent@host",
	}
	enqueued, promotion, err := s.EnqueuePromotionRun(ctx, runSpec, promotionSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	// Fail the run but leave the promotion pending (simulates partial processing).
	now = now.Add(time.Minute)
	if _, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{
		Status: model.RunFailed, Phase: "test", Error: "tests failed",
	}); err != nil {
		t.Fatal(err)
	}
	// The promotion was already finished by FinishRun (since trigger=promotion).
	// Verify it was terminated.
	prom, err := s.Promotion(ctx, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prom.Status != model.PromotionFailed {
		t.Fatalf("promotion after FinishRun = %s, want failed", prom.Status)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart: the promotion is already terminal, nothing to recover.
	now = now.Add(time.Minute)
	s, err = Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	recoveredProm, err := s.Promotion(ctx, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredProm.Status != model.PromotionFailed {
		t.Fatalf("recovered promotion = %s", recoveredProm.Status)
	}
}

// ---------------------------------------------------------------------------
// FinishRun failure tail
// ---------------------------------------------------------------------------

func TestFinishRunWithFailureTail(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	enqueued, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/failure-tail", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	finished, err := s.FinishRun(ctx, enqueued.ID, model.RunResult{
		Status:      model.RunFailed,
		Phase:       "test",
		FailedBurn:  "test",
		FailedStep:  "unit",
		Error:       "tests failed",
		FailureTail: "--- FAIL: TestSomething\nExpected 1, got 2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished.FailedBurn != "test" || finished.FailedStep != "unit" {
		t.Fatalf("finished run = %#v", finished)
	}
}

// ---------------------------------------------------------------------------
// PutStepResult with start/finish times
// ---------------------------------------------------------------------------

func TestPutStepResultWithTimes(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/step-times", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	startedAt := now
	finishedAt := now.Add(5 * time.Second)
	result, err := s.PutStepResult(ctx, model.StepResult{
		RunID: run.ID, Burn: "test", Step: "unit", Ordinal: 0,
		Status: model.StepPassed, ExitCode: 0, LogStart: 0, LogEnd: 100,
		DeclaredSize: "S", StartedAt: &startedAt, FinishedAt: &finishedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.StartedAt == nil || result.FinishedAt == nil {
		t.Fatal("step result should have start/finish times")
	}
}

// ---------------------------------------------------------------------------
// EnqueueReceiveEvent with release_delivered outcome
// ---------------------------------------------------------------------------

func TestRecordReceiveEventReleaseDelivered(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := s.RecordReceiveEvent(ctx, model.ReceiveEventSpec{
		ID: "release-delivered-1", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefTag, Ref: "v1.0.0", Outcome: "release_delivered",
		ObjectSHA: sha, CommitSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// CreateGenesis nil context
// ---------------------------------------------------------------------------

func TestCreateGenesisNilContext(t *testing.T) {
	if _, err := CreateGenesis(nil, "/path", Options{}); !errors.Is(err, ErrInvalid) { //nolint:staticcheck
		t.Fatalf("nil context = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// CreateGenesis empty path
// ---------------------------------------------------------------------------

func TestCreateGenesisEmptyPath(t *testing.T) {
	if _, err := CreateGenesis(context.Background(), "", Options{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty path = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// EnqueueReceiveEvent with tag+release (exercises matchReceiveRun tag branch)
// ---------------------------------------------------------------------------

func TestEnqueueReceiveEventTagRelease(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	objectSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	event := model.ReceiveEventSpec{
		ID: "tag-release-1", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefTag, Ref: "v2.0.0",
		ObjectSHA: objectSHA, CommitSHA: commitSHA, Outcome: "queued",
	}
	runSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefTag, Ref: "v2.0.0",
		SHA: objectSHA, TestedSHA: commitSHA, Actor: "agent@host",
		Trigger: "tag", Release: true,
	}
	result, err := s.EnqueueReceiveEvent(ctx, event, runSpec)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Release || result.Ref != "v2.0.0" {
		t.Fatalf("tag release run = %#v", result.Run)
	}
}

// ---------------------------------------------------------------------------
// matchReceiveRun: branch run with release=true is rejected
// ---------------------------------------------------------------------------

func TestEnqueueReceiveEventBranchReleaseMismatch(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	event := model.ReceiveEventSpec{
		ID: "branch-release-1", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefBranch, Ref: "feature/x",
		ObjectSHA: sha, CommitSHA: sha, Outcome: "queued",
	}
	runSpec := model.RunSpec{
		RepoID: repo.ID, RefKind: model.RefBranch, Ref: "feature/x",
		SHA: sha, TestedSHA: sha, Actor: "agent@host",
		Trigger: "branch", Release: true, // release on a branch
	}
	if _, err := s.EnqueueReceiveEvent(ctx, event, runSpec); !errors.Is(err, ErrInvalid) {
		t.Fatalf("branch release = %v, want ErrInvalid", err)
	}
}

// ---------------------------------------------------------------------------
// UpstreamByName not-found
// ---------------------------------------------------------------------------

func TestUpstreamByNameNotFound(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.UpstreamByName(ctx, "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpstreamByName(missing) = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Upstream lookup not found
// ---------------------------------------------------------------------------

func TestUpstreamNotFound(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	if _, err := s.Upstream(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Upstream(missing) = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// StepResults empty run
// ---------------------------------------------------------------------------

func TestStepResultsEmpty(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	results, err := s.StepResults(ctx, "nonexistent-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("expected empty step results, got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// PutStepResult upsert (update existing step result)
// ---------------------------------------------------------------------------

func TestPutStepResultUpsert(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/upsert-step", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	// First insert.
	if _, err := s.PutStepResult(ctx, model.StepResult{
		RunID: run.ID, Burn: "test", Step: "unit", Ordinal: 0,
		Status: model.StepPassed, ExitCode: 0, LogStart: 0, LogEnd: 100,
		DeclaredSize: "S",
	}); err != nil {
		t.Fatal(err)
	}
	// Upsert: same burn+step, different data.
	now = now.Add(time.Second)
	result, err := s.PutStepResult(ctx, model.StepResult{
		RunID: run.ID, Burn: "test", Step: "unit", Ordinal: 1,
		Status: model.StepFailed, ExitCode: 1, LogStart: 0, LogEnd: 200,
		DeclaredSize: "M",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != model.StepFailed || result.ExitCode != 1 || result.LogEnd != 200 {
		t.Fatalf("upserted step result = %#v", result)
	}
	// Only one step result for the burn+step.
	steps, err := s.StepResults(ctx, run.ID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("step results count = %d, %v", len(steps), err)
	}
}

// ---------------------------------------------------------------------------
// ReceiveEventSpec with OldSHA present
// ---------------------------------------------------------------------------

func TestRecordReceiveEventWithOldSHA(t *testing.T) {
	now := time.Date(2026, 8, 17, 14, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	repo := createRepo(t, s)
	ctx := context.Background()

	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := s.RecordReceiveEvent(ctx, model.ReceiveEventSpec{
		ID: "event-old-sha", Actor: "agent@host", RepoID: repo.ID,
		RefKind: model.RefBranch, Ref: "feature/old", Outcome: "deleted",
		OldSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSameNameRepositoriesAcrossUpstreams proves the #264 store contract end
// to end: registration of a duplicate name under a second upstream succeeds
// (compound UNIQUE(upstream_id, name)), bare-name resolution and removal
// refuse with ErrAmbiguous, and qualified selectors resolve and remove
// exactly one of the pair.
func TestSameNameRepositoriesAcrossUpstreams(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	s := testStore(t, &now)
	ctx := context.Background()

	github, err := s.RegisterUpstream(ctx, "admin@host", model.UpstreamSpec{
		Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/oberthci",
	})
	if err != nil {
		t.Fatal(err)
	}
	codeberg, err := s.RegisterUpstream(ctx, "admin@host", model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/cloudtaser",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRepository(ctx, "admin@host", model.RepositorySpec{
		Name: "terraform", UpstreamID: codeberg.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterRepository(ctx, "admin@host", model.RepositorySpec{
		Name: "terraform", UpstreamID: github.ID, DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("second upstream registration of the same name must succeed (issue #264): %v", err)
	}

	// Bare-name resolution is ambiguous and must refuse.
	if _, err := s.RepositoryByName(ctx, "terraform"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("bare lookup = %v, want ErrAmbiguous", err)
	}
	// Qualified selectors resolve their own repository.
	githubRepo, err := s.RepositoryByName(ctx, "github/oberthci/terraform")
	if err != nil || githubRepo.UpstreamID != github.ID {
		t.Fatalf("github-qualified lookup = %+v, %v", githubRepo, err)
	}
	codebergRepo, err := s.RepositoryByName(ctx, "cloudtaser/terraform")
	if err != nil || codebergRepo.UpstreamID != codeberg.ID {
		t.Fatalf("org-qualified lookup = %+v, %v", codebergRepo, err)
	}

	// Bare removal must refuse rather than delete an arbitrary row.
	if _, err := s.RemoveRepository(ctx, "admin@host", "terraform"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("bare removal = %v, want ErrAmbiguous", err)
	}
	// Qualified removal removes exactly the named pair's repository.
	removed, err := s.RemoveRepository(ctx, "admin@host", "github/oberthci/terraform")
	if err != nil {
		t.Fatal(err)
	}
	if removed.UpstreamID != github.ID || removed.UpstreamName != "github" || removed.UpstreamOrg != "oberthci" {
		t.Fatalf("removed = %+v", removed)
	}
	survivor, err := s.RepositoryByName(ctx, "terraform")
	if err != nil || survivor.UpstreamID != codeberg.ID {
		t.Fatalf("survivor lookup = %+v, %v (bare name is unique again)", survivor, err)
	}
}
