package argojob_test

import (
	"context"
	"fmt"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"testing"
	"time"
)

// The native commit survives a lost create reply and an initially absent GET.
type delayedNativeRead struct {
	*fake.Workflows
	reads int
	hide  bool
}

func (f *delayedNativeRead) Create(ctx context.Context, w *wf.Workflow, o meta.CreateOptions) (*wf.Workflow, error) {
	if _, err := f.Workflows.Create(ctx, w, o); err != nil {
		return nil, err
	}
	return nil, apierrors.NewTimeoutError("create reply lost", 1)
}
func (f *delayedNativeRead) Get(ctx context.Context, name string, o meta.GetOptions) (*wf.Workflow, error) {
	f.reads++
	if f.hide && f.reads == 1 {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "workflows"}, name)
	}
	return f.Workflows.Get(ctx, name, o)
}
func TestFindSettlesLateVisibleCreateWithoutAnotherPost(t *testing.T) {
	t.Parallel()
	for _, hide := range []bool{false, true} {
		t.Run(fmt.Sprint(hide), func(t *testing.T) {
			p, _, native := fixture(t)
			s := memoryAdmission(t, p)
			f := &delayedNativeRead{Workflows: native, hide: hide}
			e := argojob.New(argojob.Config{Workflows: f, Runs: s})
			b, err := e.Create(t.Context(), p)
			if !hide {
				compositionOK(t, err)
				if b.Workflow.UID == "" || native.Posts != 1 || f.reads != 1 {
					t.Fatal("immediate read lost UID")
				}
				return
			}
			if err == nil {
				t.Fatal("initial missing read hidden")
			}
			r, err := s.Run(t.Context(), p.View().RunID)
			compositionOK(t, err)
			b, found, err := e.Find(t.Context(), r)
			compositionOK(t, err)
			if !found || native.Posts != 1 || b.Workflow.UID == "" || f.reads != 2 {
				t.Fatal("late create not settled", b, found, native.Posts, f.reads)
			}
		})
	}
}

func TestFindAbsentWorkflowLeavesRunUnbound(t *testing.T) {
	t.Parallel()
	p, _, f := fixture(t)
	s := memoryAdmission(t, p)
	r, err := s.Run(t.Context(), p.View().RunID)
	compositionOK(t, err)
	b, found, err := argojob.New(argojob.Config{Workflows: f, Runs: s}).Find(t.Context(), r)
	compositionOK(t, err)
	if found || b != (nativebinding.RunBinding{}) || f.Gets != 1 || f.Posts != 0 {
		t.Fatal("absent Workflow reported as found or submitted", b, found, f.Gets, f.Posts)
	}
	stored, err := s.Run(t.Context(), r.ID)
	compositionOK(t, err)
	if stored.Origin.WorkflowUID != "" || stored.Status != r.Status {
		t.Fatal("absent Workflow changed durable run", stored.Origin, stored.Status)
	}
}

type podReadCounter struct {
	fake.Cache
	reads int
}

func (c *podReadCounter) Pods(name string) []*core.Pod { c.reads++; return c.Cache.Pods(name) }
func TestHoldsReadsOnePodSnapshotForTwoThousandNodes(t *testing.T) {
	t.Parallel()
	_, f, b := submitted(t)
	f.Object.Status.Nodes = wf.Nodes{}
	cache := &podReadCounter{}
	for i := range 2000 {
		n := nativeNode(fmt.Sprint(i), ".build", wf.NodeSucceeded, int64(i))
		n.Name = b.Workflow.Name + fmt.Sprintf(".fan(%d:item)", i)
		f.Object.Status.Nodes[n.ID] = n
		cache.PodObjects = append(cache.PodObjects, nativePod(f.Object, n, 0))
	}
	cache.WorkflowObject = f.Object
	e := argojob.New(argojob.Config{Workflows: f, Cache: cache, Now: func() time.Time { return time.Unix(100, 0) }})
	holds, err := e.Holds(t.Context(), b)
	compositionOK(t, err)
	if cache.reads != 1 || len(holds) != 0 {
		t.Fatal("repeated Pod snapshot", cache.reads, len(holds))
	}
}
func TestFindRefusesForeignRunLabelAndSpecSeal(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"run", "seal", "namespace"} {
		t.Run(field, func(t *testing.T) {
			p, _, f := fixture(t)
			s := memoryAdmission(t, p)
			_, err := argojob.New(argojob.Config{Workflows: f, Runs: bindingRefusal{s}}).Create(t.Context(), p)
			if err == nil {
				t.Fatal("fixture bound")
			}
			switch field {
			case "run":
				f.Object.Labels[nativebinding.RunLabel] = "foreign"
			case "namespace":
				f.Object.Namespace = "foreign"
			default:
				f.Object.Annotations[nativebinding.IdentityAnnotation] = "foreign"
			}
			r, err := s.Run(t.Context(), p.View().RunID)
			compositionOK(t, err)
			_, found, err := argojob.New(argojob.Config{Workflows: f, Runs: s}).Find(t.Context(), r)
			if err == nil || found || f.Posts != 1 {
				t.Fatal("foreign workflow adopted", err, found)
			}
			stored, err := s.Run(t.Context(), r.ID)
			compositionOK(t, err)
			if stored.Origin.WorkflowUID != "" {
				t.Fatal("foreign UID persisted")
			}
		})
	}
}
