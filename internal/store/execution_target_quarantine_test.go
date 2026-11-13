package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestTargetPlacementMigrationQuarantinesInconsistentCustody(t *testing.T) {
	for _, kind := range []string{"missing", "different", "malformed", "consistent", "no-resources"} {
		t.Run(kind, func(t *testing.T) {
			s := newMigrationIDTestStore(t)
			ctx := t.Context()
			index := -1
			for i, m := range migrations {
				if m.id == "20261113004204-remove-execution-target-placement-9b77efae7729ff92178a88a4344e38e4" {
					index = i
				}
			}
			if index < 0 {
				t.Fatal("missing migration")
			}
			if err := s.migrateWithMigrations(ctx, nil, migrations[:index]); err != nil {
				t.Fatal(err)
			}
			if err := s.configureLiveReaders(ctx, 0); err != nil {
				t.Fatal(err)
			}
			repo := createRepo(t, s)
			run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/target", strings.Repeat("a", 40)))
			if err != nil {
				t.Fatal(err)
			}
			snapshot := json.RawMessage(`{"Config":{"ExecutionPlacement":{"node_name":"old-node","namespace":"remote"}}}`)
			binding := model.ExecutionTargetBinding{RunID: run.ID, Workflow: "old-workflow", Target: "remote", RepositoryID: repo.ID, Namespace: "remote", Snapshot: snapshot}
			raw, _ := json.Marshal(binding)
			if _, err = s.db.Exec(`INSERT INTO execution_target_bindings(run_id,workflow_name,binding_json,admission_json) VALUES(?,?,?,'{}')`, run.ID, binding.Workflow, string(raw)); err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				if kind == "no-resources" {
					break
				}
				annotation := map[string]string{"oberth.ci/execution-target": strings.Repeat("a", 64)}
				if kind == "missing" && i == 1 {
					delete(annotation, "oberth.ci/execution-target")
				}
				if kind == "different" && i == 1 {
					annotation["oberth.ci/execution-target"] = strings.Repeat("b", 64)
				}
				if kind == "malformed" {
					annotation["oberth.ci/execution-target"] = "invalid"
				}
				body, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotation}})
				name := []string{"source", "workflow"}[i]
				if _, err = s.db.Exec(`INSERT INTO execution_target_resources(run_id,kind,namespace,name,uid,object_json,not_before) VALUES(?,'Pod','remote',?,'original-uid',?,1)`, run.ID, name, string(body)); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.ExecutionTargetResources(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			audit, err := s.VerifyAuditChain(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err = s.migrateWithMigrations(ctx, nil, migrations); err != nil {
					t.Fatal("migration aborted startup", err)
				}
			}
			quarantined := kind != "consistent" && kind != "no-resources"
			got, found, err := s.ExecutionTarget(ctx, run.ID, binding.Workflow)
			if !found {
				t.Fatal("retained binding vanished")
			}
			if quarantined {
				if !errors.Is(err, ErrInvalidState) || !errors.Is(err, ErrExecutionTargetQuarantined) {
					t.Fatal("inconsistent binding usable", err)
				}
				var saved string
				if err = s.db.QueryRow(`SELECT binding_json FROM execution_target_bindings WHERE run_id=?`, run.ID).Scan(&saved); err != nil || saved != string(raw) {
					t.Fatal("inconsistent evidence rewritten", err)
				}
				status, err := s.ExecutionTargetRecoveryStatus(ctx)
				if err != nil || status == nil || status.QuarantinedBindings != 1 || status.Remedy != model.ExecutionTargetRecoveryRemedy {
					t.Fatal(status, err)
				}
				if _, err = s.PrepareExecutionTargetResource(ctx, model.ExecutionTargetResource{RunID: run.ID, Kind: "Pod", Namespace: "remote", Name: "new", Object: json.RawMessage("{}"), NotBefore: time.Now()}); !errors.Is(err, ErrInvalidState) {
					t.Fatal("quarantine authorized create", err)
				}
				if err = s.BindExecutionTargetAdmission(ctx, run.ID, []byte("{}")); err == nil {
					t.Fatal("quarantine authorized admission")
				}
				if err = s.ObserveExecutionTargetUID(ctx, run.ID, "Pod", "remote", "source", "original-uid"); !errors.Is(err, ErrInvalidState) {
					t.Fatal("quarantine authorized UID adoption", err)
				}
				if _, err = s.db.Exec(`UPDATE execution_target_bindings SET quarantine_reason='' WHERE run_id=?`, run.ID); err == nil {
					t.Fatal("quarantine could be cleared")
				}
			} else {
				if err != nil || strings.Contains(string(got.Snapshot), "node_name") {
					t.Fatal("consistent binding not converted", got, err)
				}
				status, err := s.ExecutionTargetRecoveryStatus(ctx)
				if err != nil || status != nil {
					t.Fatal("consistent binding quarantined", status, err)
				}
			}
			if _, err = s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/unrelated", strings.Repeat("c", 40))); err != nil {
				t.Fatal("quarantine blocked unrelated work", err)
			}
			after, err := s.ExecutionTargetResources(ctx, run.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("resource evidence changed", err)
			}
			afterAudit, err := s.VerifyAuditChain(ctx)
			if err != nil || !reflect.DeepEqual(audit, afterAudit) {
				t.Fatal("audit changed", err)
			}
		})
	}
}
