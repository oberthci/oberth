package store

import (
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestResourceCeilingValidationScopeArchiveAndRevocation(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	ctx := t.Context()
	up, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "example", Kind: "ssh", BaseURL: "ssh://example.invalid/org"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "a", UpstreamID: up.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "b", UpstreamID: up.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	custom, err := ParseResourceCeiling(map[string]string{"cpu": "1250m", "memory": "2Gi"}, map[string]string{"cpu": "3", "memory": "5Gi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetResourceCeiling(ctx, a.ID, "admin", &custom); err != nil {
		t.Fatal(err)
	}
	other, err := s.ResourceCeiling(ctx, b.ID)
	if err != nil || other.Requests.Cpu().MilliValue() != 4000 {
		t.Fatal("numeric grant escaped repository scope")
	}
	for _, cpu := range []string{"", "bad", "-1", "0", "9"} {
		if _, err = ParseResourceCeiling(map[string]string{"cpu": cpu, "memory": "2Gi"}, map[string]string{"cpu": "3", "memory": "5Gi"}); err == nil {
			t.Fatalf("invalid CPU %q accepted", cpu)
		}
	}
	invalid := *custom.DeepCopy()
	delete(invalid.Limits, "memory")
	if _, err = s.SetResourceCeiling(ctx, a.ID, "admin", &invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid direct grant: %v", err)
	}
	if _, err = s.SetResourceCeiling(ctx, b.ID+999, "admin", &custom); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown repository: %v", err)
	}
	if _, err = s.SetRepositoryArchived(ctx, "admin", a.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetResourceCeiling(ctx, a.ID, "admin", &custom); !errors.Is(err, ErrRepositoryArchived) {
		t.Fatalf("archived grant accepted: %v", err)
	}
	if _, err = s.SetResourceCeiling(ctx, a.ID, "admin", nil); err != nil {
		t.Fatalf("archived grant cannot be revoked: %v", err)
	}
	got, err := s.ResourceCeiling(ctx, a.ID)
	if err != nil || got.Requests.Cpu().MilliValue() != 4000 {
		t.Fatalf("revocation did not restore defaults: %v", err)
	}
	if _, err = s.SetResourceCeiling(ctx, a.ID, "admin", nil); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("repeated revoke: %v", err)
	}
	var count int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_actions WHERE action IN ('resource.grant','resource.revoke')`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("grant/revoke audit count %d: %v", count, err)
	}
}
