package store

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testdb"
	"github.com/oberthci/oberth/pkg/pipeline"
)

const resourceCeilingDowngradeSQL = testdb.PostV48Downgrade + `DROP TABLE IF EXISTS resource_ceilings; DELETE FROM schema_migrations WHERE version>=48;`

func TestStage2ResourceCeilingMigration(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	up, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "example", Kind: "ssh", BaseURL: "ssh://example.invalid/org"})
	if err != nil {
		t.Fatal(err)
	}
	repos := map[string]int64{}
	for _, state := range []string{"L", "XL", "revoked", "absent"} {
		repo, err := s.CreateRepository(ctx, model.RepositorySpec{Name: state, UpstreamID: up.ID, DefaultBranch: "main"})
		if err != nil {
			t.Fatal(err)
		}
		repos[state] = repo.ID
		if state != "absent" {
			size := pipeline.XL
			if state == "L" {
				size = pipeline.L
			}
			if _, err = s.SetSizeGrant(ctx, repo.ID, "admin", size); err != nil {
				t.Fatal(err)
			}
			if state == "revoked" {
				if _, err = s.SetSizeGrant(ctx, repo.ID, "admin", ""); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// Reconstruct the previous schema ledger and exercise the production runner.
	if _, err = s.db.ExecContext(ctx, resourceCeilingDowngradeSQL); err != nil {
		t.Fatal(err)
	}
	var previous, versions int
	if err = s.db.QueryRowContext(ctx, `SELECT MAX(version), COUNT(*) FROM schema_migrations`).Scan(&previous, &versions); err != nil || previous != 47 || versions != 47 {
		t.Fatalf("upgrade must start from contiguous v47: max=%d count=%d err=%v", previous, versions, err)
	}
	if err = s.migrate(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM resource_ceilings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("migrated %d records, want active and revoked grants", count)
	}
	for state, id := range repos {
		ceiling, err := s.ResourceCeiling(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		cpu, mem, storage := int64(4000), int64(8<<30), int64(16<<30)
		if state == "XL" {
			cpu, mem, storage = 8000, 16<<30, 32<<30
		}
		for _, values := range []corev1.ResourceList{ceiling.Requests, ceiling.Limits} {
			if values.Cpu().MilliValue() != cpu || values.Memory().Value() != mem || values.StorageEphemeral().Value() != storage {
				t.Fatalf("%s migrated incorrectly: %+v", state, ceiling)
			}
		}
		if state != "absent" {
			grant, err := s.ResourceGrant(ctx, id)
			old, oldErr := s.SizeGrant(ctx, id)
			if err != nil || oldErr != nil || grant.GrantedBy != old.GrantedBy || !grant.GrantedAt.Equal(old.GrantedAt) || grant.RevokedBy != old.RevokedBy || !reflect.DeepEqual(grant.RevokedAt, old.RevokedAt) {
				t.Fatalf("%s lost provenance: %+v %v", state, grant, err)
			}
		}
	}
	// An operator's custom numeric ceiling survives rerunning/reopening migration.
	custom, err := ParseResourceCeiling(map[string]string{"cpu": "1250m", "memory": "2Gi"}, map[string]string{"cpu": "3", "memory": "5Gi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetResourceCeiling(ctx, repos["XL"], "admin-new", &custom); err != nil {
		t.Fatal(err)
	}
	if err = s.migrate(ctx, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResourceCeiling(ctx, repos["XL"])
	if err != nil || !reflect.DeepEqual(got, custom) {
		t.Fatalf("migration overwrote custom ceiling: %+v %v", got, err)
	}
	// Reopen through the production store path; no credentials or cluster needed.
	reopened, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err = reopened.ResourceCeiling(ctx, repos["XL"])
	if err != nil || !reflect.DeepEqual(got, custom) {
		t.Fatalf("reopen changed numeric ceiling: %v", err)
	}
	// Audit records carry structured numeric resources.
	var details string
	if err = s.db.QueryRowContext(ctx, `SELECT details FROM audit_actions WHERE action='resource.grant' ORDER BY id DESC LIMIT 1`).Scan(&details); err != nil {
		t.Fatal(err)
	}
	var audit map[string]any
	if err = json.Unmarshal([]byte(details), &audit); err != nil || audit["resources"] == nil {
		t.Fatalf("missing numeric audit: %s", details)
	}

}
