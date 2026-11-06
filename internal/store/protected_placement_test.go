package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/protectedtests"
)

func placedRuntimeFixture(t *testing.T, s *Store) (protectedtests.GenericAdmission, protectedtests.PlacementAdmission) {
	t.Helper()
	a := genericRuntimeFixture(t, s)
	a.Intent.WorkflowName = "native-parent-trusted"
	run, err := s.Run(t.Context(), a.Intent.RunID)
	requireStoreNoError(t, err)
	scope := protectedtests.PlacementScope{Schema: "oberth.execution-placement/protected-v2", RunID: a.Intent.RunID, Repository: a.Intent.Repository, RepositoryID: a.Intent.RepoID, Ref: run.Ref, SHA: a.Intent.CandidateSHA, SourceDigest: a.Intent.WorkflowSpecDigest, PipelineFile: ".oberth/build.yaml", Namespace: a.Intent.Namespace, ServiceAccount: "pipeline-runner", NormalWorkflow: "native-parent", PreflightWorkflow: a.Intent.WorkflowName, Expires: a.Intent.Deadline.Add(time.Minute), Policy: protectedtests.PlacementPolicy{BaselineSHA: a.Intent.BaselineSHA, Contract: a.Intent.Contract, Recipe: a.Intent.Recipe, InventoryDigest: a.Intent.InventoryDigest, CaseCount: a.Intent.CaseCount, CapabilityDigest: protectedtests.CapabilityDigest(a.Capability), SupplementalGroup: a.SupplementalGroup, MaxPhaseSeconds: 3600}}
	placement, err := json.Marshal(scope)
	requireStoreNoError(t, err)
	config, err := json.Marshal(struct {
		ExecutionPlacement     json.RawMessage
		PipelineServiceAccount string
	}{placement, "pipeline-runner"})
	requireStoreNoError(t, err)
	p, err := protectedtests.NewPlacementAdmission(placement, config, a)
	requireStoreNoError(t, err)
	return a, p
}

func TestProtectedPlacementAtomicAuditAndReopen(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	a, p := placedRuntimeFixture(t, s)
	ctx := t.Context()
	_, err := s.db.Exec(`CREATE TRIGGER fail_placed_audit BEFORE INSERT ON audit_actions WHEN NEW.action='protected.placement-admitted' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	requireStoreNoError(t, err)
	if _, err = s.ReservePlacedGenericPreflight(ctx, a, p); err == nil {
		t.Fatal("failed audit admitted placement")
	}
	var count int
	requireStoreNoError(t, s.db.QueryRow(`SELECT (SELECT count(*) FROM vm_executions)+(SELECT count(*) FROM protected_runtime_admissions)+(SELECT count(*) FROM protected_placement_admissions)+(SELECT count(*) FROM vm_capacity_slots WHERE run_id IS NOT NULL)`).Scan(&count))
	if count != 0 {
		t.Fatal("failed transaction retained custody/capacity")
	}
	_, err = s.db.Exec(`DROP TRIGGER fail_placed_audit`)
	requireStoreNoError(t, err)
	_, err = s.ReservePlacedGenericPreflight(ctx, a, p)
	requireStoreNoError(t, err)
	_, err = s.ReservePlacedGenericPreflight(ctx, a, p)
	requireStoreNoError(t, err)
	if _, err = s.ReserveGenericPreflight(ctx, a); err == nil {
		t.Fatal("placement lost on idempotent reserve")
	}
	for _, sql := range []string{`UPDATE protected_placement_admissions SET admission_json='{}'`, `DELETE FROM protected_placement_admissions`, `INSERT OR REPLACE INTO protected_placement_admissions SELECT * FROM protected_placement_admissions`} {
		if _, err = s.db.Exec(sql); err == nil {
			t.Fatal("mutable placement custody")
		}
	}

	// Source custody must be durable before a seed/Workflow POST, immutable and
	// rolled back when its chained audit cannot be appended.
	_, err = s.db.Exec(`CREATE TRIGGER fail_source_audit BEFORE INSERT ON audit_actions WHEN NEW.action='protected.placement-source' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	requireStoreNoError(t, err)
	if err = s.BindPlacementSource(ctx, a.Intent.RunID, a.Intent.WorkflowName, "original-source-uid"); err == nil {
		t.Fatal("unaudited source custody admitted")
	}
	sourceUID, err := s.PlacementSourceUID(ctx, a.Intent.RunID, a.Intent.WorkflowName)
	requireStoreNoError(t, err)
	if sourceUID != "" {
		t.Fatal("failed audit retained source custody")
	}
	_, err = s.db.Exec(`DROP TRIGGER fail_source_audit`)
	requireStoreNoError(t, err)
	requireStoreNoError(t, s.BindPlacementSource(ctx, a.Intent.RunID, a.Intent.WorkflowName, "original-source-uid"))
	requireStoreNoError(t, s.BindPlacementSource(ctx, a.Intent.RunID, a.Intent.WorkflowName, "original-source-uid"))
	if err = s.BindPlacementSource(ctx, a.Intent.RunID, a.Intent.WorkflowName, "replacement-source-uid"); err == nil {
		t.Fatal("source UID replaced")
	}
	if err = s.BindPlacementSource(ctx, a.Intent.RunID, "foreign-phase", "foreign-uid"); err == nil {
		t.Fatal("foreign source phase admitted")
	}
	path := s.livePath
	requireStoreNoError(t, s.Close())
	s = openTestStore(t, path, &now)

	sourceUID, err = s.PlacementSourceUID(ctx, a.Intent.RunID, a.Intent.WorkflowName)
	requireStoreNoError(t, err)
	if sourceUID != "original-source-uid" {
		t.Fatal("restart lost source custody")
	}
	got, err := s.PlacementAdmission(ctx, a.Intent.RunID)
	requireStoreNoError(t, err)
	if got == nil || got.Digest() != p.Digest() {
		t.Fatal("reopen lost placement custody")
	}
	_, err = s.VerifyAuditState(ctx)
	requireStoreNoError(t, err)
	now = now.Add(3 * time.Minute)
	if err = s.CheckGenericPreflightAuthority(ctx, a.Intent.RunID); err == nil {
		t.Fatal("expired custody authorized execution")
	}
	got, err = s.PlacementAdmission(ctx, a.Intent.RunID)
	requireStoreNoError(t, err)
	if got == nil {
		t.Fatal("expiry discarded cleanup custody")
	}
}
