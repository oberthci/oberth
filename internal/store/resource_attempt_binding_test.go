package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

// Exercise separately persisted legacy observation and new inventory records.
func TestReviewAttemptCoverageBindingAndLegacyUnknown(t *testing.T) {
	for _, kind := range []string{"covered", "legacy", "workflow", "namespace", "node", "pod", "stage", "step", "template"} {
		t.Run(kind, func(t *testing.T) {
			s, run, at := resourceStoreFixture(t)
			v := coveredResourceRow(run.ID, at)
			if err := s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); err != nil {
				t.Fatal(err)
			}
			i := model.ResourceAttemptInventory{RunID: run.ID, WorkflowName: v.WorkflowName, WorkflowUID: v.WorkflowUID, Namespace: v.Namespace, Complete: true, Terminal: true, Attempts: []model.ResourceAttempt{{NodeID: v.NodeID, Stage: v.Stage, Step: v.Step, Template: v.TemplateName, Status: model.StepPassed, PodUID: v.PodUID}}}
			switch kind {
			case "workflow":
				i.WorkflowUID = "33333333-3333-4333-8333-333333333333"
			case "namespace":
				i.Namespace = "other"
			case "node":
				i.Attempts[0].NodeID = "other-node"
			case "pod":
				i.Attempts[0].PodUID = "33333333-3333-4333-8333-333333333333"
			case "stage":
				i.Attempts[0].Stage = "other-stage"
			case "step":
				i.Attempts[0].Step = "other-step"
			case "template":
				i.Attempts[0].Template = "other-template"
			}
			if kind == "covered" {
				if err := s.RecordResourceAttempts(t.Context(), i); err != nil {
					t.Fatal(err)
				}
			} else if kind != "legacy" {
				// Reconstruct already accepted legacy records independently of
				// new first-write guards; the production reader must still fence them.
				putLegacyAttemptInventory(t, s, run.RepoID, i)
			}
			if _, err := s.db.Exec(`UPDATE runs SET status='passed' WHERE id=?`, run.ID); err != nil {
				t.Fatal(err)
			}
			h, err := s.RunResources(t.Context(), run.RepoID, RunResourceFilter{RunID: run.ID, Runs: 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(h.Observations) != 1 {
				t.Fatal("lost retained metrics")
			}
			if kind == "covered" {
				if h.SafetyIncomplete || len(h.AttemptGaps) != 0 {
					t.Fatal("covered control refused", h)
				}
				return
			}
			// Every invalid binding must fence the retained metric's own invocation,
			// either globally or through an exact gap for that invocation.
			fenced := h.SafetyIncomplete
			for _, g := range h.AttemptGaps {
				if g.Stage == v.Stage && g.Template == v.TemplateName {
					fenced = true
				}
			}
			if !fenced {
				t.Fatalf("%s contradiction does not fence retained invocation %s/%s: incomplete=%t gaps=%+v", kind, v.Stage, v.TemplateName, h.SafetyIncomplete, h.AttemptGaps)
			}
		})
	}
}

func putLegacyAttemptInventory(t *testing.T, s *Store, repoID int64, i model.ResourceAttemptInventory) {
	t.Helper()
	if i.Validate() != nil {
		t.Fatal("invalid legacy inventory fixture")
	}
	raw, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO resource_attempt_inventories(run_id,repo_id,observation_json) VALUES(?,?,?)`, i.RunID, repoID, string(raw)); err != nil {
		t.Fatal(err)
	}
	for _, a := range i.Attempts {
		raw, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec(`INSERT INTO resource_attempts(run_id,node_id,observation_json) VALUES(?,?,?)`, i.RunID, a.NodeID, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAttemptFirstWriteReconcilesUsageInBothOrders(t *testing.T) {
	for _, order := range []string{"usage-first", "inventory-first"} {
		for _, kind := range []string{"consistent", "unbound-pod", "unrelated-node", "workflow", "namespace", "pod", "stage", "step", "template", "owner"} {
			t.Run(order+"/"+kind, func(t *testing.T) {
				s, run, at := resourceStoreFixture(t)
				v := coveredResourceRow(run.ID, at)
				i := model.ResourceAttemptInventory{RunID: v.RunID, WorkflowName: v.WorkflowName, WorkflowUID: v.WorkflowUID, Namespace: v.Namespace, Complete: true, Terminal: true, Attempts: []model.ResourceAttempt{{NodeID: v.NodeID, Stage: v.Stage, Step: v.Step, Template: v.TemplateName, Owner: v.FragmentRepository, PodUID: v.PodUID, Status: model.StepPassed}}}
				switch kind {
				case "unbound-pod":
					i.Attempts[0].PodUID = ""
				case "unrelated-node":
					i.Attempts[0].NodeID = "other-node"
				case "workflow":
					i.WorkflowUID = "33333333-3333-4333-8333-333333333333"
				case "namespace":
					i.Namespace = "other"
				case "pod":
					i.Attempts[0].PodUID = "33333333-3333-4333-8333-333333333333"
				case "stage":
					i.Attempts[0].Stage = "other"
				case "step":
					i.Attempts[0].Step = "other"
				case "template":
					i.Attempts[0].Template = "other"
				case "owner":
					i.Attempts[0].Owner = "other"
				}
				var err error
				if order == "usage-first" {
					if err = s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); err != nil {
						t.Fatal(err)
					}
					err = s.RecordResourceAttempts(t.Context(), i)
				} else {
					if err = s.RecordResourceAttempts(t.Context(), i); err != nil {
						t.Fatal(err)
					}
					err = s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v})
				}
				conflict := kind != "consistent" && kind != "unbound-pod" && kind != "unrelated-node"
				if conflict && !errors.Is(err, ErrInvalid) {
					t.Fatalf("accepted first-write %s: %v", kind, err)
				}
				if !conflict && err != nil {
					t.Fatalf("positive distinct/unknown binding refused: %v", err)
				}
			})
		}
	}
}
