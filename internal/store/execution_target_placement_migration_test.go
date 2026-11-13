package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestExecutionTargetPlacementMigrationPreservesCustody(t *testing.T) {
	const id = "20261113004204-remove-execution-target-placement-9b77efae7729ff92178a88a4344e38e4"
	s := newMigrationIDTestStore(t)
	ctx := t.Context()
	index := -1
	for i, m := range migrations {
		if m.id == id {
			index = i
		}
	}
	if index < 0 {
		t.Fatal("migration missing")
	}
	if err := s.migrateWithMigrations(ctx, nil, migrations[:index]); err != nil {
		t.Fatal(err)
	}
	if err := s.configureLiveReaders(ctx, 0); err != nil {
		t.Fatal(err)
	}
	repo := createRepo(t, s)
	spec := testRunSpec(repo.ID, "feature/target", strings.Repeat("a", 40))
	spec.Trigger = "branch"
	result, err := s.EnqueueRun(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.SetRunWorkflowName(ctx, result.ID, "old-workflow")
	if err != nil {
		t.Fatal(err)
	}
	qualified, err := s.QualifiedRepoName(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := json.RawMessage(`{"Version":1,"Config":{"Namespace":"remote","Other":9007199254740993,"ExecutionPlacement":{"schema":"oberth.execution-placement/branch-v2","node_name":"old-node","node_uid":"node-uid","provider_id":"cloud/node","zone":"old-zone","pool":"old-pool","taint_key":"dedicated","taint_value":"trusted","node_version":"approved-admission-version","namespace":"remote","endpoint_identity":"unchanged"}},"Cleanup":{"Pod":"seed"}}`)
	b := model.ExecutionTargetBinding{RunID: run.ID, Workflow: run.WorkflowName, Target: "remote", Repository: qualified, RepositoryID: repo.ID, RefKind: string(run.RefKind), Trigger: run.Trigger, Ref: run.Ref, SHA: run.SHA, TestedSHA: run.SHA, Namespace: "remote", Snapshot: snapshot}
	body, _ := json.Marshal(b)
	if _, err := s.db.Exec(`INSERT INTO execution_target_bindings(run_id,workflow_name,binding_json) VALUES(?,?,?)`, b.RunID, b.Workflow, string(body)); err != nil {
		t.Fatal(err)
	}
	admission := []byte(`{"workflow":{"metadata":{"annotations":{"oberth.ci/execution-placement":"original-authority"}}},"identity":"unchanged"}`)
	if _, err := s.db.Exec(`UPDATE execution_target_bindings SET admission_json=? WHERE run_id=?`, string(admission), b.RunID); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(b)
	hash := sha256.Sum256(original)
	digest := hex.EncodeToString(hash[:])
	resource := model.ExecutionTargetResource{RunID: b.RunID, Kind: "Workflow", Namespace: b.Namespace, Name: b.Workflow, NotBefore: time.Now().Add(time.Minute), Object: json.RawMessage(`{"metadata":{"annotations":{"oberth.ci/execution-target":"` + digest + `","oberth.ci/execution-placement":"original-authority"}},"spec":{"nodeSelector":{"pool":"old-pool"}}}`)}
	if _, err := s.db.Exec(`INSERT INTO execution_target_resources(run_id,kind,namespace,name,uid,object_json,not_before) VALUES(?,?,?,?,?,?,?)`, b.RunID, resource.Kind, resource.Namespace, resource.Name, "original-uid", string(resource.Object), resource.NotBefore.UnixNano()); err != nil {
		t.Fatal(err)
	}
	beforeResources, err := s.ExecutionTargetResources(ctx, b.RunID)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledger := releaseSplitLedger(t, s.db)
	for range 2 {
		if err := s.migrateWithMigrations(ctx, nil, migrations); err != nil {
			t.Fatal(err)
		}
	}
	saved, found, err := s.ExecutionTarget(ctx, b.RunID, b.Workflow)
	if err != nil || !found {
		t.Fatal(saved, found, err)
	}
	if saved.CustodyDigest != digest {
		t.Fatal("original object binding lost", saved.CustodyDigest, digest)
	}
	var config struct {
		Config struct {
			ExecutionPlacement map[string]json.RawMessage
			Other              json.Number
		}
		Cleanup map[string]string
	}
	if err := json.Unmarshal(saved.Snapshot, &config); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"node_name", "node_uid", "provider_id", "zone", "pool", "taint_key", "taint_value"} {
		if _, ok := config.Config.ExecutionPlacement[field]; ok {
			t.Fatal("retired configuration survived", field)
		}
	}
	if config.Config.Other.String() != "9007199254740993" || string(config.Config.ExecutionPlacement["endpoint_identity"]) != `"unchanged"` || config.Cleanup["Pod"] != "seed" {
		t.Fatal("unrelated configuration changed", string(saved.Snapshot))
	}
	afterResources, err := s.ExecutionTargetResources(ctx, b.RunID)
	if err != nil || !reflect.DeepEqual(beforeResources, afterResources) {
		t.Fatal("immutable intents or UIDs changed", err)
	}
	var afterAdmission string
	if err := s.db.QueryRow(`SELECT admission_json FROM execution_target_bindings WHERE run_id=?`, b.RunID).Scan(&afterAdmission); err != nil || afterAdmission != string(admission) {
		t.Fatal("admission capsule changed", afterAdmission, err)
	}
	afterAudit, err := s.VerifyAuditChain(ctx)
	if err != nil || !reflect.DeepEqual(audit, afterAudit) {
		t.Fatal("audit history changed", err)
	}
	for prior, stamp := range ledger {
		if releaseSplitLedger(t, s.db)[prior] != stamp {
			t.Fatal("old migration receipt changed", prior)
		}
	}
	if first, err := s.PrepareExecutionTargetResource(ctx, resource); err != nil || first {
		t.Fatal("migration authorized another POST", first, err)
	}
	if err := s.BindExecutionTarget(ctx, saved); err != nil {
		t.Fatal("migrated binding not idempotent", err)
	}
	if _, err := s.db.Exec(`UPDATE execution_target_bindings SET custody_digest=? WHERE run_id=?`, strings.Repeat("f", 64), b.RunID); err == nil {
		t.Fatal("custody digest mutable")
	}
}
