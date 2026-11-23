//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"reflect"
	"testing"

	argologging "github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	"github.com/oberthci/oberth/internal/stateport"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This proves scheduled admission custody, without firing or executing a step.
func TestComposedScheduledChildRecordedOnceFromRealPreparation(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareScheduledObservation(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	parent := f.scheduledParent(t, commit)
	ctx := argologging.WithLogger(t.Context(), scheduledFixtureLogger)
	child := common.ConvertCronWorkflowToWorkflowWithProperties(ctx, parent, parent.Name+"-456", f.clock.Now())
	child.Namespace = f.config.PipelineNamespace
	child = initializedScheduledObservation(t, child, f.clock.Now())
	if !reflect.DeepEqual(child.Spec, parent.Spec.WorkflowSpec) {
		t.Fatal("started child changed sealed scheduled spec")
	}
	child, err := f.argo.ArgoprojV1alpha1().Workflows(child.Namespace).Create(t.Context(), child, meta.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.ID == child.Name && r.Origin.WorkflowUID == string(child.UID) })
	proof := f.scheduledChildProof(t, run, commit)
	f.replayScheduledObservation(t, child, proof)
	f.checkOnce(t)
	events, err := f.owners.c.store.Events(t.Context(), stateport.EventFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events.Events {
		if event.RunID != run.ID {
			continue
		}
		if event.Kind != "run.updated" {
			t.Fatal("unexpected scheduled child event", event)
		}
		if event.Summary == "scheduled run observed" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("scheduled child admission repeated its event", count)
	}
}

func (f *composedFixture) scheduledChildProof(t *testing.T, run stateport.Run, commit string) nativebinding.ChildBinding {
	t.Helper()
	if run.CommitSHA == nil || *run.CommitSHA != commit || run.Origin.Actor != "oberth:scheduler" || run.Origin.ScheduleEntry != "nightly" {
		t.Fatal("scheduled run lost admission facts", run.Origin)
	}
	proof, err := f.owners.c.store.ChildProof(t.Context(), run.ID)
	if err != nil || proof.Run.Workflow.UID != run.Origin.WorkflowUID || proof.Run.Identity != run.Admission.Digest {
		t.Fatal("child proof differs", proof, err)
	}

	return proof
}

func (f *composedFixture) replayScheduledObservation(t *testing.T, child *wf.Workflow, proof nativebinding.ChildBinding) {
	t.Helper()
	adapter := scheduleChildren{f.owners.c}
	for range 3 {
		_, found, _, err := adapter.ChildBinding(t.Context(), child)
		if err != nil || !found {
			t.Fatal("retained child binding missing", err)
		}
		if err = adapter.VerifyChild(child, proof); err != nil {
			t.Fatal(err)
		}
		if _, err = f.argo.ArgoprojV1alpha1().Workflows(child.Namespace).Update(t.Context(), child, meta.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}
