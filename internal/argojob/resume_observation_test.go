package argojob_test

import (
	"context"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
)

func TestResumeInformerLag(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		for _, snapshot := range []string{"current", "predecessor", "foreign-live"} {
			name := snapshot + "/apply"
			if restart {
				name = snapshot + "/restart"
			}
			t.Run(name, func(t *testing.T) { checkResumeInformerLag(t, restart, snapshot) })
		}
	}
}

func checkResumeInformerLag(t *testing.T, restart bool, snapshot string) {
	t.Helper()
	s, in, native, pods := resumeRecoveryFixture(t)
	cache := &fake.Cache{WorkflowObject: native.Object.DeepCopy()}
	config := resumeRecoveryConfig(s, native, pods)
	config.Cache = cache
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	config.Reporter = &reportSpy{FinishedFunc: func(ctx context.Context, id string, o argojob.Observation) error {
		err := saveResumeFinish(ctx, s, id, o)
		cancel()
		return err
	}}
	e := argojob.New(config)
	compositionOK(t, e.ApplyResume(t.Context(), in.RunID))
	native.Object.Status = wf.WorkflowStatus{Phase: wf.WorkflowSucceeded}
	if snapshot == "current" {
		cache.WorkflowObject = native.Object.DeepCopy()
	}
	want := stepmodel.RunPassed
	if snapshot == "foreign-live" {
		native.Object.Labels[nativebinding.RunLabel] = "foreign"
		want = stepmodel.RunFailed
	}
	if restart {
		e = argojob.New(config)
	}
	_ = e.Run(ctx)
	r, err := s.Run(t.Context(), in.RunID)
	compositionOK(t, err)
	if r.Status != want {
		t.Fatalf("ownership observation: status=%s reason=%q; want %s", r.Status, r.DisplayReason, want)
	}
}

func saveResumeFinish(ctx context.Context, s *store.Store, id string, o argojob.Observation) error {
	r, err := s.Run(ctx, id)
	if err != nil {
		return err
	}
	return s.Finish(ctx, stateport.Action{}, stateport.RunUpdate{ID: id, Revision: r.Revision, Status: o.Status, Reason: o.Reason, Detail: o.Detail})
}
