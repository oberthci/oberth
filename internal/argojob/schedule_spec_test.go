package argojob_test

import (
	"reflect"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestScheduledChildRefusesUnstampedSpecChanges(t *testing.T) {
	t.Parallel()
	for _, replay := range []bool{false, true} {
		stage := "first-binding"
		if replay {
			stage = "observation"
		}
		t.Run(stage, func(t *testing.T) { checkScheduleSpecRefusals(t, replay) })
	}
}

func checkScheduleSpecRefusals(t *testing.T, replay bool) {
	t.Helper()
	for name, change := range map[string]func(*wf.Workflow){
		"command":           func(w *wf.Workflow) { w.Spec.Templates[0].Container.Command = []string{"changed"} },
		"run-id-annotation": func(w *wf.Workflow) { w.Spec.WorkflowMetadata.Annotations[nativebinding.RunIDAnnotation] = w.Name },
		"pod-run-label":     func(w *wf.Workflow) { w.Spec.PodMetadata.Labels[nativebinding.RunLabel] = w.Name },
		"extra-parameter": func(w *wf.Workflow) {
			w.Spec.Arguments.Parameters = append(w.Spec.Arguments.Parameters, wf.Parameter{Name: "oberth-extra", Value: wf.AnyStringPtr("value")})
		},
	} {
		t.Run(name, func(t *testing.T) { assertScheduleSpecRefused(t, replay, change) })
	}
}

func assertScheduleSpecRefused(t *testing.T, replay bool, change func(*wf.Workflow)) {
	t.Helper()
	f := realScheduleFixture(t)
	f.child = initializeScheduleChild(t, f.child)
	if replay {
		compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	}
	change(f.child)
	if err := f.executor().RecordChildForTest(t.Context(), f.child); err == nil {
		t.Fatal("unstamped spec change accepted")
	}
	runs, err := f.s.Runs(t.Context(), stateport.RunFilter{Limit: 10})
	compositionOK(t, err)
	want := 0
	if replay {
		want = 1
	}
	if len(runs) != want {
		t.Fatal("refused spec changed durable run count", len(runs), want)
	}
}

func TestScheduledChildKeepsSealedSpecAcrossNativeShutdown(t *testing.T) {
	t.Parallel()
	f := realScheduleFixture(t)
	retained, err := f.s.ScheduleAdmission(t.Context(), f.configRepositoryID(t), "nightly", f.prepared.View().Digest)
	compositionOK(t, err)
	f.child.Spec.Shutdown = wf.ShutdownStrategyStop
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	f.child.Spec.Shutdown = wf.ShutdownStrategyTerminate
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	assertScheduleRecord(t, f, retained)
	run, err := f.s.Run(t.Context(), f.child.Name)
	compositionOK(t, err)
	if !reflect.DeepEqual(run.Admission.Workflow.Spec, retained.Workflow.Spec) {
		t.Fatal("native control replaced sealed admission")
	}
}
