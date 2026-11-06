package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestProtectedParentWorkflowCustodyAuditAndRestart(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	ctx := t.Context()
	runtime, placement := placedRuntimeFixture(t, s)
	id := runtime.Intent.RunID
	name := "native-parent"
	_, err := s.ReservePlacedGenericPreflight(ctx, runtime, placement)
	requireStoreNoError(t, err)
	requireStoreNoError(t, s.SubmitKVMPreflight(ctx, id))
	requireStoreNoError(t, s.BindKVMPreflight(ctx, id, "workflow-uid"))
	requireStoreNoError(t, s.RecordKVMReceipt(ctx, id, kvmReceipt(runtime.Intent)))
	requireStoreNoError(t, s.CompleteKVMPreflightCleanup(ctx, id, "workflow-uid"))
	body, err := json.Marshal(map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Workflow", "metadata": map[string]any{"name": name, "namespace": runtime.Intent.Namespace, "annotations": map[string]string{"oberth.ci/run-id": id, "oberth.ci/spec-identity": strings.Repeat("a", 64)}}, "spec": map[string]string{"entrypoint": "original"}})
	requireStoreNoError(t, err)
	if err = s.PreparePlacementWorkflow(ctx, id, name, body); err == nil {
		t.Fatal("parent intent without source UID admitted")
	}
	requireStoreNoError(t, s.BindPlacementSource(ctx, id, name, "original-source-uid"))
	if err = s.BindPlacementWorkflowUID(ctx, id, name, "original-parent-uid"); err == nil {
		t.Fatal("UID without pre-POST intent admitted")
	}
	_, err = s.db.Exec(`CREATE TRIGGER fail_parent_audit BEFORE INSERT ON audit_actions WHEN NEW.action='protected.placement-workflow' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	requireStoreNoError(t, err)
	if err = s.PreparePlacementWorkflow(ctx, id, name, body); err == nil {
		t.Fatal("unaudited parent intent admitted")
	}
	retained, uid, err := s.PlacementWorkflow(ctx, id, name)
	requireStoreNoError(t, err)
	if len(retained) != 0 || uid != "" {
		t.Fatal("failed intent append retained authority")
	}
	_, err = s.db.Exec(`DROP TRIGGER fail_parent_audit`)
	requireStoreNoError(t, err)
	requireStoreNoError(t, s.PreparePlacementWorkflow(ctx, id, name, body))
	requireStoreNoError(t, s.PreparePlacementWorkflow(ctx, id, name, body))
	if err = s.PreparePlacementWorkflow(ctx, id, name, []byte(strings.Replace(string(body), "original", "replacement", 1))); err == nil {
		t.Fatal("parent intent replaced")
	}
	_, err = s.db.Exec(`CREATE TRIGGER fail_parent_uid BEFORE INSERT ON audit_actions WHEN NEW.action='protected.placement-workflow-bound' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	requireStoreNoError(t, err)
	if err = s.BindPlacementWorkflowUID(ctx, id, name, "original-parent-uid"); err == nil {
		t.Fatal("unaudited parent UID admitted")
	}
	_, uid, err = s.PlacementWorkflow(ctx, id, name)
	requireStoreNoError(t, err)
	if uid != "" {
		t.Fatal("failed UID append retained binding")
	}
	_, err = s.db.Exec(`DROP TRIGGER fail_parent_uid`)
	requireStoreNoError(t, err)
	requireStoreNoError(t, s.BindPlacementWorkflowUID(ctx, id, name, "original-parent-uid"))
	requireStoreNoError(t, s.BindPlacementWorkflowUID(ctx, id, name, "original-parent-uid"))
	if err = s.BindPlacementWorkflowUID(ctx, id, name, "replacement-parent-uid"); err == nil {
		t.Fatal("original parent UID replaced")
	}
	path := s.livePath
	requireStoreNoError(t, s.Close())
	s = openTestStore(t, path, &now)
	retained, uid, err = s.PlacementWorkflow(ctx, id, name)
	requireStoreNoError(t, err)
	if string(retained) != string(body) || uid != "original-parent-uid" {
		t.Fatal("restart changed parent custody")
	}
	_, err = s.SetWorkflowCapabilityGrant(ctx, runtime.Intent.RepoID, "operator", nil)
	requireStoreNoError(t, err)
	if err = s.PreparePlacementWorkflow(ctx, id, name, body); err == nil {
		t.Fatal("revoked grant authorized another POST")
	}
	requireStoreNoError(t, s.BindPlacementWorkflowUID(ctx, id, name, "original-parent-uid"))
	if _, _, err = s.PlacementWorkflow(ctx, id, name); err != nil {
		t.Fatal("revocation lost cleanup custody", err)
	}
	_, err = s.VerifyAuditState(ctx)
	requireStoreNoError(t, err)
}
