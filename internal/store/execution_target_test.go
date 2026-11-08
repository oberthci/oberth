package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/testdb"
)

func targetStoreFixture(t *testing.T) (*Store, model.ExecutionTargetBinding) {
	t.Helper()
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	spec := testRunSpec(repo.ID, "feature", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	spec.Trigger = "branch"
	result, err := s.EnqueueRun(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.SetRunWorkflowName(t.Context(), result.ID, "target-workflow")
	if err != nil {
		t.Fatal(err)
	}
	qualified, err := s.QualifiedRepoName(t.Context(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	b := model.ExecutionTargetBinding{RunID: run.ID, Workflow: run.WorkflowName, Target: "remote", Repository: qualified, RepositoryID: repo.ID, RefKind: string(run.RefKind), Trigger: run.Trigger, ScheduleEntry: run.ScheduleEntry, Ref: run.Ref, SHA: run.SHA, TestedSHA: run.SHA, Namespace: "remote", Snapshot: json.RawMessage(`{"version":1}`)}
	return s, b
}

func TestExecutionTargetMigrationAndProcessRecoveryPreserveCanonicalState(t *testing.T) {
	s, b := targetStoreFixture(t)
	ctx := t.Context()
	if _, err := s.db.Exec(testdb.PostExecutionTargetsDowngrade); err != nil {
		t.Fatal(err)
	}
	before := releaseSplitLedger(t, s.db)
	if err := s.migrate(ctx, nil); err != nil {
		t.Fatal(err)
	}
	after := releaseSplitLedger(t, s.db)
	// Five target-custody receipts plus every later migration replayed by
	// the cumulative downgrade (branch deletions, runtime overrides,
	// assisted runs).
	if len(after) != len(before)+8 {
		t.Fatal("unexpected migration authority")
	}
	var newReceipts int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE id IN ('20261105050000-execution-target-custody','20261105180000-pinned-source-bindings','20261106021113-protected-runtime-admissions-6bb6bdbe716e4f2623808c4dc612d943','20261106033000-protected-placement-admissions-96f3e81aca97478ea8d7499e6e378631','20261106053000-protected-workflow-custody-f83c6b27a4904dc8bd29e1952e86430a')`).Scan(&newReceipts); err != nil || newReceipts != 5 {
		t.Fatalf("new receipts=%d: %v", newReceipts, err)
	}
	for id, stamp := range before {
		if after[id] != stamp {
			t.Fatal("old migration receipt rewritten", id)
		}
	}
	if err := s.BindExecutionTarget(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.BindExecutionTargetAdmission(ctx, b.RunID, []byte(`{"admitted":true}`)); err != nil {
		t.Fatal(err)
	}
	r := model.ExecutionTargetResource{RunID: b.RunID, Kind: "Pod", Namespace: b.Namespace, Name: "seed", Object: json.RawMessage(`{}`), NotBefore: time.Now().Add(time.Minute)}
	if first, err := s.PrepareExecutionTargetResource(ctx, r); err != nil || !first {
		t.Fatal(first, err)
	}
	if err := s.ObserveExecutionTargetUID(ctx, b.RunID, r.Kind, r.Namespace, r.Name, "original"); err != nil {
		t.Fatal(err)
	}
	var sequence int
	var name, path string
	if err := s.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenCurrent(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	saved, found, err := reopened.ExecutionTarget(ctx, "", b.Workflow)
	if err != nil || !found || saved.RunID != b.RunID || saved.Target != b.Target {
		t.Fatal(saved, found, err)
	}
	if first, err := reopened.PrepareExecutionTargetResource(ctx, r); err != nil || first {
		t.Fatal("process restart authorized another POST", first, err)
	}
	rows, err := reopened.ExecutionTargetResources(ctx, b.RunID)
	if err != nil || len(rows) != 1 || rows[0].UID != "original" {
		t.Fatal("process restart lost original UID", rows, err)
	}
}

func TestExecutionTargetBindingCanonicalImmutableAndRecoverable(t *testing.T) {
	s, b := targetStoreFixture(t)
	ctx := t.Context()
	for name, change := range map[string]func(*model.ExecutionTargetBinding){"repo": func(b *model.ExecutionTargetBinding) { b.RepositoryID++ }, "qualified repo": func(b *model.ExecutionTargetBinding) { b.Repository = "other/org/repo" }, "sha": func(b *model.ExecutionTargetBinding) { b.SHA = "wrong" }, "tested sha": func(b *model.ExecutionTargetBinding) { b.TestedSHA = "wrong" }, "workflow": func(b *model.ExecutionTargetBinding) { b.Workflow = "other" }, "ref": func(b *model.ExecutionTargetBinding) { b.Ref = "other" }} {
		t.Run(name, func(t *testing.T) {
			copy := b
			change(&copy)
			if !errors.Is(s.BindExecutionTarget(ctx, copy), ErrInvalid) {
				t.Fatal("noncanonical binding accepted")
			}
		})
	}
	for range 2 {
		if err := s.BindExecutionTarget(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	copy := b
	copy.Target = "other"
	if !errors.Is(s.BindExecutionTarget(ctx, copy), ErrInvalid) {
		t.Fatal("route rebound")
	}
	for _, lookup := range []struct{ run, name string }{{b.RunID, ""}, {"", b.Workflow}} {
		got, found, err := s.ExecutionTarget(ctx, lookup.run, lookup.name)
		if err != nil || !found || string(got.Snapshot) != string(b.Snapshot) || got.Target != b.Target {
			t.Fatalf("recovery: %#v %v", got, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE execution_target_bindings SET binding_json='{}' WHERE run_id=?`, b.RunID); err == nil {
		t.Fatal("SQL route mutation accepted")
	}
}

func TestExecutionTargetPrePOSTCustodyCancellationAndUIDFence(t *testing.T) {
	s, b := targetStoreFixture(t)
	ctx := t.Context()
	if err := s.BindExecutionTarget(ctx, b); err != nil {
		t.Fatal(err)
	}
	r := model.ExecutionTargetResource{RunID: b.RunID, Kind: "PersistentVolumeClaim", Namespace: b.Namespace, Name: "source", Object: json.RawMessage(`{"metadata":{"name":"source"}}`), NotBefore: time.Now().Add(time.Minute)}
	if _, err := s.PrepareExecutionTargetResource(ctx, r); !errors.Is(err, ErrInvalid) {
		t.Fatal("allocation without admission allowed")
	}
	for range 2 {
		if err := s.BindExecutionTargetAdmission(ctx, b.RunID, []byte(`{"ceiling":"original"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.BindExecutionTargetAdmission(ctx, b.RunID, []byte(`{"ceiling":"changed"}`)); !errors.Is(err, ErrInvalid) {
		t.Fatal("admission rebound")
	}
	for i := range 2 {
		first, err := s.PrepareExecutionTargetResource(ctx, r)
		if err != nil || first != (i == 0) {
			t.Fatalf("POST permission=%v %v", first, err)
		}
	}
	r.Object = json.RawMessage(`{}`)
	if _, err := s.PrepareExecutionTargetResource(ctx, r); !errors.Is(err, ErrInvalid) {
		t.Fatal("create intent mutated")
	}
	for range 2 {
		if err := s.ObserveExecutionTargetUID(ctx, b.RunID, r.Kind, r.Namespace, r.Name, "original-uid"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ObserveExecutionTargetUID(ctx, b.RunID, r.Kind, r.Namespace, r.Name, "replacement-uid"); !errors.Is(err, ErrInvalid) {
		t.Fatal("UID rebound")
	}
	if err := s.CancelExecutionTarget(ctx, b.RunID); err != nil {
		t.Fatal(err)
	}
	r.Name = "second"
	if _, err := s.PrepareExecutionTargetResource(ctx, r); !errors.Is(err, ErrInvalid) {
		t.Fatal("cancelled run authorized POST")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runs WHERE id=?`, b.RunID); err == nil {
		t.Fatal("run purged with unresolved resource custody")
	}
	if err := s.CompleteExecutionTargetCleanup(ctx, b.RunID); err != nil {
		t.Fatal(err)
	}
	resources, err := s.ExecutionTargetResources(ctx, b.RunID)
	if err != nil || len(resources) != 0 {
		t.Fatal("confirmed inventory not released", err)
	}
	var receipt string
	if err := s.db.QueryRowContext(ctx, `SELECT cleanup_receipt FROM execution_target_bindings WHERE run_id=?`, b.RunID).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Resources []model.ExecutionTargetResource
	}
	if json.Unmarshal([]byte(receipt), &decoded) != nil || len(decoded.Resources) != 1 || decoded.Resources[0].UID != "original-uid" {
		t.Fatal("original UID inventory lost")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE execution_target_bindings SET cancelled=0 WHERE run_id=?`, b.RunID); err == nil {
		t.Fatal("cancellation revoked")
	}
	if _, err := s.db.ExecContext(context.Background(), `DELETE FROM runs WHERE id=?`, b.RunID); err != nil {
		t.Fatal("confirmed cleanup still fences retention", err)
	}
}
