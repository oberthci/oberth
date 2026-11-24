package argojob_test

import (
	"context"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/runlog"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"reflect"
	"testing"
	"time"
)

func TestAuthoredPodGCUsesWorkflowTTLWithoutRestoration(t *testing.T) {
	t.Parallel()
	prepared, _, _ := fixtureKind(t, "gc-policy")
	w := prepared.View().Workflow
	if w.Spec.PodGC == nil {
		t.Fatal("controller defaults can restore premature Pod GC")
	}
	selector, err := meta.LabelSelectorAsSelector(w.Spec.PodGC.LabelSelector)
	compositionOK(t, err)
	if selector.Matches(labels.Set{}) || selector.Matches(labels.Set{"oberth.ci/archive-held": "any"}) {
		t.Fatal("Pod GC can destroy unarchived sources", w.Spec.PodGC)
	}
	db, f, _, pod := collectionFixture(t)
	f.Object.Spec.PodGC = w.Spec.PodGC.DeepCopy()
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	compositionOK(t, err)
	defer func() { _ = logs.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	restored := false
	native := collectionNative{Workflows: f, collected: func(after *wf.Workflow) {
		restored = reflect.DeepEqual(after.Spec.PodGC, w.Spec.PodGC)
		cancel()
	}}
	reports := 1
	_ = argojob.New(argojob.Config{Runs: db, Progress: db, Logs: logs, Workflows: native, Pods: &fake.Pods{Object: pod}, Cache: &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{pod}}, Reporter: collectionReporter(t, db, f, &reports), Now: compositionClock}).Run(ctx)
	if !restored {
		t.Fatal("archive completion rewrote Pod GC after Argo stopped reconciling", ctx.Err())
	}
}
