package argojob_test

import (
	"context"
	"errors"
	argofake "github.com/argoproj/argo-workflows/v4/pkg/client/clientset/versioned/fake"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func submitted(t *testing.T) (*argojob.Executor, *fake.Workflows, nativebinding.RunBinding) {
	t.Helper()
	p, e, f := fixture(t)
	b, err := e.Create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	return e, f, b
}
func TestSubmitOnce(t *testing.T) {
	t.Parallel()
	p, e, f := fixture(t)
	if _, err := e.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Create(t.Context(), p); err != nil || f.Posts != 2 || f.Gets != 1 {
		t.Fatalf("AlreadyExists %v posts=%d gets=%d", err, f.Posts, f.Gets)
	}
	f.Object.Annotations[nativebinding.RunIDAnnotation] = "foreign"
	if _, err := e.Create(t.Context(), p); err == nil {
		t.Fatal("foreign Workflow bound")
	}
}
func TestUncertainCreateReadsOnceNeverPostsAgain(t *testing.T) {
	t.Parallel()
	p, e, f := fixture(t)
	f.CreateError = apierrors.NewTimeoutError("uncertain", 1)
	if _, err := e.Create(t.Context(), p); err == nil || f.Posts != 1 || f.Gets != 1 {
		t.Fatalf("uncertain %v %+v", err, f)
	}
}
func TestAdoptAbsentOnlyOnNotFound(t *testing.T) {
	t.Parallel()
	e, f, b := submitted(t)
	f.Object = nil
	if _, err := e.Adopt(t.Context(), b); !errors.As(err, new(argojob.ErrAbsent)) {
		t.Fatalf("absent: %v", err)
	}
	f.GetError = context.DeadlineExceeded
	if _, err := e.Adopt(t.Context(), b); err == nil || errors.As(err, new(argojob.ErrAbsent)) {
		t.Fatalf("outage: %v", err)
	}
}
func TestCancelStopNeverOverTerminate(t *testing.T) {
	t.Parallel()
	e, f, b := submitted(t)
	for _, stop := range []func(context.Context, nativebinding.RunBinding) error{e.Stop, e.Terminate, e.Stop} {
		if err := stop(t.Context(), b); err != nil {
			t.Fatal(err)
		}
	}
	if string(f.Object.Spec.Shutdown) != "Terminate" || f.Patches != 2 {
		t.Fatal("shutdown downgraded")
	}
	b.Workflow.UID = "replacement"
	if err := e.Terminate(t.Context(), b); err == nil {
		t.Fatal("replacement stopped")
	}
}

func TestCancelPatchFencesConcurrentReplacementAndShutdown(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"uid", "shutdown"} {
		t.Run(change, func(t *testing.T) {
			e, f, b := submitted(t)
			f.BeforePatch = func() {
				if change == "uid" {
					f.Object.UID = "replacement"
				} else {
					f.Object.ResourceVersion = "2"
					f.Object.Spec.Shutdown = "Terminate"
				}
			}
			if err := e.Stop(t.Context(), b); err == nil {
				t.Fatal("concurrent native change overwritten")
			}
		})
	}
}

func TestSubmitPreservesAuthoredMetadataAndSealedSpec(t *testing.T) {
	t.Parallel()
	p, e, f := fixture(t)
	before := p.View()
	if _, err := e.Create(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if f.Object.Labels["pipeline"] != "authored" || f.Object.Annotations["note"] != "authored" {
		t.Fatal("authored metadata lost")
	}
	if !reflect.DeepEqual(f.Object.Spec, before.Workflow.Spec) {
		t.Fatal("sealed spec changed")
	}
}

func TestNativeWorkflowClientCancellation(t *testing.T) {
	t.Parallel()
	_, f, b := submitted(t)
	native := argofake.NewSimpleClientset(f.Object.DeepCopy())
	client := argojob.WorkflowClient{NativeWorkflows: native.ArgoprojV1alpha1().Workflows(b.Workflow.Namespace)}
	e := argojob.New(argojob.Config{Workflows: client})
	if err := e.Stop(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	got, err := client.Get(t.Context(), b.Workflow.Name, meta.GetOptions{})
	if err != nil || string(got.Spec.Shutdown) != "Stop" {
		t.Fatal("native adapter did not patch", err)
	}
}

func TestNativeRunLabelAndForbiddenCreateAreRefused(t *testing.T) {
	t.Parallel()
	e, f, b := submitted(t)
	f.Object.Labels[nativebinding.RunLabel] = "foreign"
	if _, err := e.Adopt(t.Context(), b); err == nil {
		t.Fatal("foreign run label accepted")
	}
	p, e, f := fixture(t)
	f.CreateError = apierrors.NewForbidden(schema.GroupResource{Resource: "workflows"}, nativebinding.WorkflowName(p.View().RunID), errors.New(`User "server" cannot create resource "workflows" in API group "argoproj.io" in the namespace "ci"`))
	if _, err := e.Create(t.Context(), p); err == nil || f.Gets != 0 {
		t.Fatal("forbidden create read as uncertain", err, f.Gets)
	}
}

func TestObservedWorkflowMustRetainAdmittedRef(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"", "refs/heads/other"} {
		t.Run(ref, func(t *testing.T) {
			e, f, b := submitted(t)
			if _, err := e.Adopt(t.Context(), b); err != nil {
				t.Fatal("unchanged workflow refused", err)
			}
			f.Object.Annotations[nativebinding.RefAnnotation] = ref
			if _, err := e.Adopt(t.Context(), b); !errors.As(err, new(*nativebinding.Refusal)) {
				t.Fatal("adopt trusted a mismatched ref", ref, err)
			}
			if err := e.Stop(t.Context(), b); !errors.As(err, new(*nativebinding.Refusal)) || f.Patches != 0 {
				t.Fatal("shutdown trusted a mismatched ref", ref, err, f.Patches)
			}
		})
	}
}
