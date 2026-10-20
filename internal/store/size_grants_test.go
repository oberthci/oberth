package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestSizeGrantDefaultsToLAndIsScopedAuditedAndRevocable(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	database := testStore(t, &now)
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme"})
	if err != nil {
		t.Fatal(err)
	}
	ebpf, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "ebpf", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "docs", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	maximum := func(id int64) periapsis.Size {
		t.Helper()
		size, err := database.MaxRunSize(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return size
	}
	if maximum(ebpf.ID) != periapsis.L {
		t.Fatal("an ungranted repository may exceed L")
	}
	if _, err := database.SizeGrant(ctx, ebpf.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing grant: %v", err)
	}
	for _, refused := range []periapsis.Size{periapsis.S, periapsis.M, "XXL"} {
		if _, err := database.SetSizeGrant(ctx, ebpf.ID, "admin@host", refused); !errors.Is(err, ErrInvalid) {
			t.Fatalf("grant %q: %v", refused, err)
		}
	}
	grant, err := database.SetSizeGrant(ctx, ebpf.ID, "admin@host", periapsis.XL)
	if err != nil || grant.MaxSize != periapsis.XL || grant.GrantedBy != "admin@host" || grant.RevokedAt != nil {
		t.Fatalf("grant XL: %+v, %v", grant, err)
	}
	if maximum(ebpf.ID) != periapsis.XL || maximum(other.ID) != periapsis.L {
		t.Fatal("the grant is not scoped to its repository")
	}
	if _, err := database.SetSizeGrant(ctx, ebpf.ID, "admin@host", periapsis.XL); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("duplicate grant: %v", err)
	}
	if grant, err = database.SetSizeGrant(ctx, ebpf.ID, "admin@host", periapsis.L); err != nil || grant.MaxSize != periapsis.L {
		t.Fatalf("lower to L: %+v, %v", grant, err)
	}
	if grant, err = database.SetSizeGrant(ctx, ebpf.ID, "admin@host", ""); err != nil || grant.RevokedAt == nil {
		t.Fatalf("revoke: %+v, %v", grant, err)
	}
	if maximum(ebpf.ID) != periapsis.L {
		t.Fatal("a revoked grant still raises the maximum")
	}
	if _, err := database.SetSizeGrant(ctx, ebpf.ID, "admin@host", ""); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("revoke without a grant: %v", err)
	}
	if _, err := database.SetSizeGrant(ctx, other.ID+100, "admin@host", periapsis.XL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown repo: %v", err)
	}
	var audited int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_actions WHERE resource_type='repository' AND resource_id=? AND action IN ('size.grant','size.revoke')`, ebpf.ID).Scan(&audited); err != nil || audited != 3 {
		t.Fatalf("audited grant/regrant/revoke = %d, %v", audited, err)
	}
}
