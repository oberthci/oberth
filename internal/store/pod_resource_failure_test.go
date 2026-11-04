package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestPodFailurePersistsWithoutContainerTerminationAndIsScoped(t *testing.T) {
	s, run, at := resourceStoreFixture(t)
	f := model.PodResourceDisposition{RunID: run.ID, WorkflowName: "workflow-test", WorkflowUID: "11111111-1111-4111-8111-111111111111", Namespace: "argo", NodeID: "wf-1", PodName: "wf-1", PodUID: "22222222-2222-4222-8222-222222222222", Stage: "setup", Step: "install-tool", Template: "install-tool", Reason: "Evicted", ObservedAt: at}
	writer, ok := any(s).(interface {
		RecordPodResourceDisposition(context.Context, model.PodResourceDisposition) error
	})
	if !ok {
		t.Fatal("Pod failure cannot be persisted without a container ID")
	}
	if err := writer.RecordPodResourceDisposition(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if err := writer.RecordPodResourceDisposition(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE runs SET status='failed' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunResources(t.Context(), run.RepoID, RunResourceFilter{RunID: run.ID, Runs: 1, IncludeFailed: true})
	if err != nil {
		t.Fatal(err)
	}
	// Inspect the wire form so the regression also runs before the additive
	// history field exists; absence must fail rather than compile-fail.
	if !hasFailure(got) {
		t.Fatalf("failed Pod disappeared from history: %+v", got)
	}
	late := f
	late.Reason = "Completed"
	late.ObservedAt = late.ObservedAt.Add(time.Second)
	if err := writer.RecordPodResourceDisposition(t.Context(), late); err != nil {
		t.Fatal(err)
	}
	got, err = s.RunResources(t.Context(), run.RepoID, RunResourceFilter{RunID: run.ID, Runs: 1, IncludeFailed: true})
	if err != nil || !hasFailure(got) {
		t.Fatal("late success erased failure", err)
	}
	f.WorkflowUID = "33333333-3333-4333-8333-333333333333"
	if writer.RecordPodResourceDisposition(t.Context(), f) == nil {
		t.Fatal("failure binding changed")
	}
	f.WorkflowName = "foreign"
	if writer.RecordPodResourceDisposition(t.Context(), f) == nil {
		t.Fatal("foreign run workflow accepted")
	}
	other, err := s.RunResources(t.Context(), run.RepoID+1, RunResourceFilter{RunID: run.ID, Runs: 1, IncludeFailed: true})
	if err != nil || hasFailure(other) {
		t.Fatalf("cross-repo failure leak: %+v %v", other, err)
	}
}

func hasFailure(v RunResourceHistory) bool {
	raw, _ := json.Marshal(v)
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(raw, &doc)
	var failures []model.PodResourceDisposition
	_ = json.Unmarshal(doc["Dispositions"], &failures)
	return len(failures) == 1 && failures[0].Reason == "Evicted"
}
