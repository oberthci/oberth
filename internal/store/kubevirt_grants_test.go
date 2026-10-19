package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestKubeVirtGrantIsScopedAuditedAndRevocable(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	database := testStore(t, &now)
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "github", Kind: "ssh", BaseURL: "ssh://github.com/acme"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "one", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "two", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.KubeVirtGrant(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing grant: %v", err)
	}
	grant, err := database.SetKubeVirtGrant(ctx, first.ID, "admin@host", true)
	if err != nil {
		t.Fatal(err)
	}
	if grant.RepoID != first.ID || grant.GrantedBy != "admin@host" || grant.RevokedAt != nil {
		t.Fatalf("grant: %+v", grant)
	}
	for _, tc := range []struct {
		id   int64
		want bool
	}{{first.ID, true}, {second.ID, false}} {
		got, err := database.ActiveKubeVirtGrant(ctx, tc.id)
		if err != nil || got != tc.want {
			t.Fatalf("repo %d active = %v, %v", tc.id, got, err)
		}
	}
	if _, err := database.SetKubeVirtGrant(ctx, first.ID, "admin@host", true); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("duplicate grant: %v", err)
	}
	grant, err = database.SetKubeVirtGrant(ctx, first.ID, "admin@host", false)
	if err != nil || grant.RevokedAt == nil {
		t.Fatalf("revoke: %+v, %v", grant, err)
	}
	active, err := database.ActiveKubeVirtGrant(ctx, first.ID)
	if err != nil || active {
		t.Fatalf("revoked grant active = %v, %v", active, err)
	}
	if _, err := database.SetKubeVirtGrant(ctx, second.ID+100, "admin@host", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown repo: %v", err)
	}
	var audited int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_actions WHERE resource_type='repository' AND resource_id=? AND action IN ('kubevirt.grant','kubevirt.revoke')`, first.ID).Scan(&audited); err != nil || audited != 2 {
		t.Fatalf("audited grant/revoke = %d, %v", audited, err)
	}
}
