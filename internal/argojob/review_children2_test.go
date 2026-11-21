package argojob_test

import (
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
)

// Stop through the product on a recorded child, then observe it again.
func TestReviewStopRecordedChildThenObserve(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	c := f.executorConfig()
	native := c.Workflows.(*nativefake.Workflows)
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	proof, err := f.s.ChildProof(t.Context(), f.child.Name)
	compositionOK(t, err)
	f.child.ResourceVersion = "1"
	native.Object = f.child
	e := argojob.New(c)
	compositionOK(t, e.Stop(t.Context(), proof.Run))
	if native.Object.Spec.Shutdown != wf.ShutdownStrategyStop {
		t.Fatal("stop not patched", native.Object.Spec.Shutdown)
	}
	*f.child = *native.Object.DeepCopy()
	if _, err := e.Adopt(t.Context(), proof.Run); err != nil {
		t.Fatal("stopped child no longer observable", err)
	}
	if native.Posts != 0 {
		t.Fatal("child path posted a Workflow", native.Posts)
	}
}

// External shapes of the admission annotation: missing, empty, upper case.
func TestReviewAdmissionAnnotationShapesRefuse(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]*string{"missing": nil, "empty": new(string), "upper": nil} {
		t.Run(name, func(t *testing.T) {
			f := newChildFixture(t)
			switch name {
			case "missing":
				delete(f.child.Annotations, nativebinding.AdmissionAnnotation)
			case "upper":
				d := f.child.Annotations[nativebinding.AdmissionAnnotation]
				up := []byte(d)
				for i, b := range up {
					if b >= 'a' && b <= 'f' {
						up[i] = b - 32
					}
				}
				f.child.Annotations[nativebinding.AdmissionAnnotation] = string(up)
			default:
				f.child.Annotations[nativebinding.AdmissionAnnotation] = *value
			}
			err := f.executor().RecordChildForTest(t.Context(), f.child)
			t.Logf("%s: permanent=%v code=%v err=%v", name, publicerr.Permanent(err), publicerr.Classify(err).Code(), err)
			if err == nil || !publicerr.Permanent(err) {
				t.Fatal("malformed admission annotation is retried forever instead of refused")
			}
		})
	}
}
