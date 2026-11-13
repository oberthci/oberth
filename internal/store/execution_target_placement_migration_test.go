package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
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
	// Historical wire data is retained as a fixture, not an active surface.
	var fixture struct{ Snapshot, Admission, Resource json.RawMessage }
	raw, err := os.ReadFile("testdata/retired-target-placement.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	snapshot := fixture.Snapshot
	b := historicalTargetBinding{RunID: run.ID, Workflow: run.WorkflowName, Target: "remote", Repository: qualified, RepositoryID: repo.ID, RefKind: string(run.RefKind), Trigger: run.Trigger, Ref: run.Ref, SHA: run.SHA, TestedSHA: run.SHA, Namespace: "remote", Snapshot: snapshot}
	body, _ := json.Marshal(b)
	if _, err := s.db.Exec(`INSERT INTO execution_target_bindings(run_id,workflow_name,binding_json) VALUES(?,?,?)`, b.RunID, b.Workflow, string(body)); err != nil {
		t.Fatal(err)
	}
	admission := fixture.Admission
	if _, err := s.db.Exec(`UPDATE execution_target_bindings SET admission_json=? WHERE run_id=?`, string(admission), b.RunID); err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(b)
	hash := sha256.Sum256(original)
	digest := hex.EncodeToString(hash[:])
	resource := historicalTargetResource{RunID: b.RunID, Kind: "Workflow", Namespace: b.Namespace, Name: b.Workflow, NotBefore: time.Now().Add(time.Minute), Object: json.RawMessage(strings.ReplaceAll(string(fixture.Resource), "$BINDING_DIGEST", digest))}
	if _, err := s.db.Exec(`INSERT INTO execution_target_resources(run_id,kind,namespace,name,uid,object_json,not_before) VALUES(?,?,?,?,?,?,?)`, b.RunID, resource.Kind, resource.Namespace, resource.Name, "original-uid", string(resource.Object), resource.NotBefore.UnixNano()); err != nil {
		t.Fatal(err)
	}
	var beforeResources string
	err = s.db.QueryRow(`SELECT json_array(kind,namespace,name,uid,object_json,not_before) FROM execution_target_resources WHERE run_id=?`, b.RunID).Scan(&beforeResources)
	if err != nil {
		t.Fatal(err)
	}
	audit, err := s.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ledger := releaseSplitLedger(t, s.db)
	for range 2 {
		if err := s.migrateWithMigrations(ctx, nil, migrations[:index+1]); err != nil {
			t.Fatal(err)
		}
	}
	var saved historicalTargetBinding
	var savedBody string
	if err := s.db.QueryRow(`SELECT binding_json,custody_digest FROM execution_target_bindings WHERE run_id=?`, b.RunID).Scan(&savedBody, &saved.CustodyDigest); err != nil {
		t.Fatal(err)
	}
	digestAfter := saved.CustodyDigest
	if err := json.Unmarshal([]byte(savedBody), &saved); err != nil {
		t.Fatal(err)
	}
	saved.CustodyDigest = digestAfter
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
	var afterResources string
	err = s.db.QueryRow(`SELECT json_array(kind,namespace,name,uid,object_json,not_before) FROM execution_target_resources WHERE run_id=?`, b.RunID).Scan(&afterResources)
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
	if _, err := s.db.Exec(`UPDATE execution_target_bindings SET custody_digest=? WHERE run_id=?`, strings.Repeat("f", 64), b.RunID); err == nil {
		t.Fatal("custody digest mutable")
	}
}

// Test-only fixture for a shipped schema; no live target API is retained.
type historicalTargetBinding struct {
	RunID, Workflow, Target, Repository, RefKind, Trigger, Ref, SHA, TestedSHA, Namespace string
	RepositoryID                                                                          int64
	Snapshot                                                                              json.RawMessage
	CustodyDigest                                                                         string `json:",omitempty"`
}
type historicalTargetResource struct {
	RunID, Kind, Namespace, Name string
	NotBefore                    time.Time
	Object                       json.RawMessage
}
