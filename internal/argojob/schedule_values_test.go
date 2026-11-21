package argojob_test

import (
	"errors"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/nativebinding"
)

func TestScheduledChildRequiresFrozenValues(t *testing.T) {
	t.Parallel()
	f := realScheduleFixture(t)
	for name, change := range map[string]func(*wf.Workflow){
		"engine":  func(w *wf.Workflow) { w.Labels[nativebinding.EngineLabel] = "foreign" },
		"trigger": func(w *wf.Workflow) { w.Labels[nativebinding.TriggerLabel] = "release" },
		"tier":    func(w *wf.Workflow) { w.Labels[nativebinding.TierLabel] = "release" },
		"correlated-tier": func(w *wf.Workflow) {
			w.Labels[nativebinding.TriggerLabel], w.Labels[nativebinding.TierLabel] = "release", "release"
		},
		"repository":       func(w *wf.Workflow) { w.Labels[nativebinding.RepositoryLabel] = "99" },
		"ref-parameter":    func(w *wf.Workflow) { setScheduleParameter(w, "oberth-ref", "refs/heads/other") },
		"commit-parameter": func(w *wf.Workflow) { setScheduleParameter(w, "oberth-sha", strings.Repeat("f", 40)) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := f.child.DeepCopy()
			change(candidate)
			_, found, _, err := f.syncer.ChildBinding(t.Context(), candidate)
			var refusal *cronsync.ChildRefusal
			if found || !errors.As(err, &refusal) {
				t.Fatal("altered value accepted", name, found, err)
			}
		})
	}
}

func TestScheduledChildCorrelatedFrozenChangesRefusedByAdmission(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"oberth-ref", "oberth-sha"} {
		t.Run(key, func(t *testing.T) {
			f := realScheduleFixture(t)
			if key == "oberth-ref" {
				f.child.Annotations[nativebinding.RefAnnotation] = "refs/heads/other"
				setScheduleParameter(f.child, key, "refs/heads/other")
			} else {
				f.child.Labels[nativebinding.CommitLabel] = strings.Repeat("f", 40)
				setScheduleParameter(f.child, key, strings.Repeat("f", 40))
			}
			if err := f.executor().RecordChildForTest(t.Context(), f.child); err == nil {
				t.Fatal("correlated frozen change recorded")
			}
		})
	}
}

func TestScheduledChildIgnoresUnrelatedOuterMetadata(t *testing.T) {
	t.Parallel()
	f := realScheduleFixture(t)
	retained, err := f.s.ScheduleAdmission(t.Context(), f.configRepositoryID(t), "nightly", f.prepared.View().Digest)
	compositionOK(t, err)
	f.child.Labels["oberth.ci/extra"] = "before"
	f.child.Annotations["oberth.ci/extra"] = "before"
	f.child.Annotations[nativebinding.IdentityAnnotation] = "unrelated"
	f.child.Annotations[nativebinding.RunIDAnnotation] = "unrelated"
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	f.child.Labels["oberth.ci/extra"] = "after"
	f.child.Annotations["oberth.ci/extra"] = "after"
	delete(f.child.Annotations, nativebinding.IdentityAnnotation)
	f.child.Annotations[nativebinding.RunIDAnnotation] = "also-unrelated"
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	assertScheduleRecord(t, f, retained)
}

func setScheduleParameter(w *wf.Workflow, name, value string) {
	for i := range w.Spec.Arguments.Parameters {
		if w.Spec.Arguments.Parameters[i].Name == name {
			w.Spec.Arguments.Parameters[i].Value = wf.AnyStringPtr(value)
		}
	}
}
