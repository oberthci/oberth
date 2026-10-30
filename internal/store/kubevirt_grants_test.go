package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
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
	grant, err := database.SetKubeVirtGrant(ctx, first.ID, "admin@host", true, KubeVirtBudget{})
	if err != nil {
		t.Fatal(err)
	}
	if grant.RepoID != first.ID || grant.GrantedBy != "admin@host" || grant.RevokedAt != nil || grant.KubeVirtBudget != DefaultKubeVirtBudget() {
		t.Fatalf("grant: %+v", grant)
	}
	for _, tc := range []struct {
		id   int64
		want bool
	}{{first.ID, true}, {second.ID, false}} {
		budget, got, err := database.ActiveKubeVirtGrant(ctx, tc.id)
		if err != nil || got != tc.want {
			t.Fatalf("repo %d active = %v, %v", tc.id, got, err)
		}
		if got && budget != DefaultKubeVirtBudget() {
			t.Fatalf("repo %d budget = %+v, want the default", tc.id, budget)
		}
	}
	if _, err := database.SetKubeVirtGrant(ctx, first.ID, "admin@host", true, DefaultKubeVirtBudget()); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("duplicate grant: %v", err)
	}
	p2p := KubeVirtBudget{MaxVMs: 3, MaxCores: 12, MaxMemoryBytes: 24 << 30}
	grant, err = database.SetKubeVirtGrant(ctx, first.ID, "admin@host", true, p2p)
	if err != nil || grant.KubeVirtBudget != p2p {
		t.Fatalf("budget change: %+v, %v", grant, err)
	}
	if budget, active, err := database.ActiveKubeVirtGrant(ctx, first.ID); err != nil || !active || budget != p2p {
		t.Fatalf("changed budget = %+v, %v, %v", budget, active, err)
	}
	grant, err = database.SetKubeVirtGrant(ctx, first.ID, "admin@host", false, KubeVirtBudget{})
	if err != nil || grant.RevokedAt == nil {
		t.Fatalf("revoke: %+v, %v", grant, err)
	}
	_, active, err := database.ActiveKubeVirtGrant(ctx, first.ID)
	if err != nil || active {
		t.Fatalf("revoked grant active = %v, %v", active, err)
	}
	if _, err := database.SetKubeVirtGrant(ctx, second.ID+100, "admin@host", true, KubeVirtBudget{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown repo: %v", err)
	}
	var audited int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_actions WHERE resource_type='repository' AND resource_id=? AND action IN ('kubevirt.grant','kubevirt.revoke')`, first.ID).Scan(&audited); err != nil || audited != 3 {
		t.Fatalf("audited grant/budget/revoke = %d, %v", audited, err)
	}
	var details string
	if err := database.db.QueryRowContext(ctx, `SELECT details FROM audit_actions WHERE resource_type='repository' AND resource_id=? AND action='kubevirt.grant' ORDER BY id DESC LIMIT 1`, first.ID).Scan(&details); err != nil ||
		!strings.Contains(details, `"budget":{"max_vms":3,"max_cores":12,"max_memory_bytes":25769803776}`) ||
		!strings.Contains(details, `"previous_budget":{"max_vms":2,"max_cores":6,"max_memory_bytes":12884901888}`) {
		t.Fatalf("budget change audit = %s, %v", details, err)
	}
}

func TestKubeVirtGrantBudgetBounds(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	database := testStore(t, &now)
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "github", Kind: "ssh", BaseURL: "ssh://github.com/acme"})
	if err != nil {
		t.Fatal(err)
	}
	repository, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "one", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []KubeVirtBudget{
		{MaxVMs: 17}, {MaxVMs: -1}, {MaxCores: 65}, {MaxMemoryBytes: 1 << 20}, {MaxMemoryBytes: 257 << 30},
	} {
		if _, err := database.SetKubeVirtGrant(ctx, repository.ID, "admin@host", true, budget); !errors.Is(err, ErrInvalid) {
			t.Errorf("budget %+v: want ErrInvalid, got %v", budget, err)
		}
	}
	// A partial budget defaults the other fields, never keeps an old value.
	grant, err := database.SetKubeVirtGrant(ctx, repository.ID, "admin@host", true, KubeVirtBudget{MaxVMs: 5})
	if err != nil || grant.KubeVirtBudget != (KubeVirtBudget{MaxVMs: 5, MaxCores: 6, MaxMemoryBytes: 12 << 30}) {
		t.Fatalf("partial budget: %+v, %v", grant, err)
	}
	if _, err := database.SetKubeVirtGrant(ctx, repository.ID, "admin@host", false, KubeVirtBudget{MaxVMs: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("revocation with budget: %v", err)
	}
	// The schema enforces the same bounds against direct writes.
	if _, err := database.db.ExecContext(ctx, `UPDATE kubevirt_grants SET max_vms=17 WHERE repo_id=?`, repository.ID); err == nil {
		t.Fatal("schema admitted max_vms=17")
	}
	if _, err := database.db.ExecContext(ctx, `UPDATE kubevirt_grants SET max_memory_bytes=? WHERE repo_id=?`, int64(257)<<30, repository.ID); err == nil {
		t.Fatal("schema admitted 257Gi")
	}
}

// Migration 25 is additive: a grant made before it keeps the budget every
// grant had then (2 VMs, 6 cores, 12Gi).
func TestMigration25GivesExistingKubeVirtGrantsTheDefaultBudget(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "oberth-v24.sqlite")
	s, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `DROP TABLE IF EXISTS run_priority_overrides; DROP TABLE IF EXISTS queue_priority_patterns; DELETE FROM schema_migrations WHERE version>=34; ALTER TABLE step_results DROP COLUMN memory_peak_bytes; DELETE FROM schema_migrations WHERE version=33; ALTER TABLE schedule_fires DROP COLUMN last_fired_at; DELETE FROM schema_migrations WHERE version=32; DROP TRIGGER run_schedule_entry_immutable; ALTER TABLE runs DROP COLUMN schedule_entry; DELETE FROM schema_migrations WHERE version=31; DROP TRIGGER run_timing_claim; DROP TRIGGER run_timing_grant; DROP TRIGGER run_timing_park; DROP TRIGGER run_timing_terminal; DROP TABLE step_timings; DROP TABLE run_admission_timings; DROP TABLE run_timings; DROP INDEX runs_admission_idx;
ALTER TABLE runs DROP COLUMN admission_sequence;
DROP TABLE kubevirt_grants;
CREATE TABLE kubevirt_grants (
    repo_id INTEGER PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    granted_by TEXT NOT NULL,
    granted_at INTEGER NOT NULL,
    revoked_by TEXT NOT NULL DEFAULT '',
    revoked_at INTEGER,
    CHECK ((revoked_at IS NULL AND revoked_by = '') OR (revoked_at IS NOT NULL AND revoked_by != ''))
);
INSERT INTO kubevirt_grants(repo_id, granted_by, granted_at) VALUES(?, 'admin@host', 1);
DELETE FROM schema_migrations WHERE version >= 25;`, repo.ID); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, path, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("v24 -> v25 upgrade: %v", err)
	}
	defer func() { _ = migrated.Close() }()
	budget, active, err := migrated.ActiveKubeVirtGrant(ctx, repo.ID)
	if err != nil || !active || budget != DefaultKubeVirtBudget() {
		t.Fatalf("pre-v25 grant after upgrade = %+v, %v, %v; want the default budget", budget, active, err)
	}
}
