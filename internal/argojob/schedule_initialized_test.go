package argojob_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	nativeTemplate "github.com/argoproj/argo-workflows/v4/util/template"
	"github.com/oberthci/oberth/internal/nativebinding"
)

// The captured objects come from the pinned controller's setExecWorkflow and
// persistUpdates. Its initialization API is private; the substitution API is public.
func TestCapturedControllerInitialization(t *testing.T) {
	t.Parallel()
	before := readScheduleNative(t, "input")
	after := readScheduleNative(t, "output")
	got := initializeScheduleChild(t, before)
	if !reflect.DeepEqual(got.Spec, after.Spec) {
		t.Fatal("native substitution differs from captured persisted spec")
	}
	for key, value := range after.Labels {
		if got.Labels[key] != value {
			t.Fatal("captured outer label differs", key)
		}
	}
	for key, value := range after.Annotations {
		if got.Annotations[key] != value {
			t.Fatal("captured outer annotation differs", key)
		}
	}
}

func TestScheduleRecordsControllerInitializedChild(t *testing.T) {
	t.Parallel()
	for _, replay := range []bool{false, true} {
		name := "first-after-initialization"
		if replay {
			name = "replay-after-initialization"
		}
		t.Run(name, func(t *testing.T) {
			f := realScheduleFixture(t)
			retained, err := f.s.ScheduleAdmission(t.Context(), f.configRepositoryID(t), "nightly", f.prepared.View().Digest)
			compositionOK(t, err)
			if replay {
				compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
			}
			f.child = initializeScheduleChild(t, f.child)
			for range 2 {
				compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
			}
			assertScheduleRecord(t, f, retained)
			run, err := f.s.Run(t.Context(), f.child.Name)
			compositionOK(t, err)
			if !reflect.DeepEqual(run.Admission.Workflow.Spec, retained.Workflow.Spec) {
				t.Fatal("record replaced the sealed spec with the observed one")
			}
			proof, err := f.s.ChildProof(t.Context(), f.child.Name)
			compositionOK(t, err)
			_, err = f.executor().Adopt(t.Context(), proof.Run)
			compositionOK(t, err)
		})
	}
}

func initializeScheduleChild(t *testing.T, child *wf.Workflow) *wf.Workflow {
	t.Helper()
	next := child.DeepCopy()
	templates := next.Spec.Templates
	next.Spec.Templates = nil
	raw, err := json.Marshal(next.Spec)
	compositionOK(t, err)
	resolved, err := nativeTemplate.Replace(t.Context(), string(raw), map[string]any{"workflow.name": next.Name}, true)
	compositionOK(t, err)
	compositionOK(t, json.Unmarshal([]byte(resolved), &next.Spec))
	next.Spec.Templates = templates
	if metadata := next.Spec.WorkflowMetadata; metadata != nil {
		for key, value := range metadata.Labels {
			next.Labels[key] = value
		}
		for key, value := range metadata.Annotations {
			next.Annotations[key] = value
		}
		if metadata.LabelsFrom[nativebinding.RunLabel].Expression == "workflow.name" {
			next.Labels[nativebinding.RunLabel] = next.Name
		}
	}
	return next
}

func readScheduleNative(t *testing.T, name string) *wf.Workflow {
	t.Helper()
	raw, err := os.ReadFile("testdata/schedule-native-" + name + ".json")
	compositionOK(t, err)
	var child wf.Workflow
	compositionOK(t, json.Unmarshal(raw, &child))
	return &child
}
