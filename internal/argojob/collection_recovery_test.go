package argojob_test

import (
	"context"
	"slices"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	core "k8s.io/api/core/v1"
)

func TestCollectionResumesAfterFinishAndBackfillsMissingTTL(t *testing.T) {
	t.Parallel()
	for _, historical := range []bool{false, true} {
		s, f, b, pod := collectionFixture(t)
		r, err := s.Run(t.Context(), b.RunID)
		compositionOK(t, err)
		compositionOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed}))
		before := collectionHistory(t, s, b.RunID)
		if historical {
			f.Object.Spec.TTLStrategy, f.Object.Finalizers = nil, nil
		} else {
			f.Object.Finalizers = append(f.Object.Finalizers, "another.ci/owner")
		}
		logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
		compositionOK(t, err)
		defer func() { compositionOK(t, logs.Close()) }()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		collected := false
		native := collectionNative{Workflows: f, collected: func(w *wf.Workflow) {
			collected = true
			if string(before) != string(collectionHistory(t, s, b.RunID)) {
				t.Fatal("collection rewrote finished history")
			}
			body, _, err := logs.Read(t.Context(), b.RunID, runlog.ReadRequest{OwnOnly: true})
			if err != nil || string(body) != "container output\n" {
				t.Fatal("collection lost archived logs", string(body), err)
			}
			if w.Spec.TTLStrategy == nil || *w.Spec.TTLStrategy.SecondsAfterCompletion != 86400 || !historical && !slices.Equal(w.Finalizers, []string{"another.ci/owner"}) {
				t.Error("collection lost TTL or another owner's finalizer", w.Spec.TTLStrategy, w.Finalizers)
			}
			cancel()
		}}
		cache := &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{pod}}
		e := argojob.New(argojob.Config{Runs: s, Progress: s, Logs: logs, Workflows: native, Pods: &fake.Pods{Object: pod}, Cache: cache, Now: compositionClock})
		cache.Changed(b.Workflow.Name)
		_ = e.Run(ctx)
		if !collected {
			t.Fatal("watch replay did not resume collection", historical, ctx.Err())
		}
	}
}

func TestCollectionPatchFencesConcurrentReplacement(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"uid", "resourceVersion"} {
		s, f, b, pod := collectionFixture(t)
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		f.BeforePatch = func() {
			if field == "uid" {
				f.Object.UID = "replacement"
			} else {
				f.Object.ResourceVersion = "replacement"
			}
			cancel()
		}
		reports := 1
		e := argojob.New(argojob.Config{Runs: s, Progress: s, Logs: fake.Logs{ArchiveFunc: func(context.Context, string, []runlog.ContainerLog) (bool, error) { return true, nil }}, Workflows: f, Cache: &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{pod}}, Now: compositionClock, Reporter: collectionReporter(t, s, f, &reports)})
		_ = e.Run(ctx)
		if f.Patches != 1 || !slices.Contains(f.Object.Finalizers, nativebinding.RecordFinalizer) {
			t.Fatal("collection patched a changed native object", field, b, f.Patches, f.Object.Finalizers)
		}
	}
}
