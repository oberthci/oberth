package argojob_test

import (
	"context"
	"errors"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func TestResumeRaces(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"changed version", "replaced Pod", "saved stop", "terminal successor", "stop after delete"} {
		t.Run(scenario, func(t *testing.T) {
			checkResumeRace(t, scenario)
		})
	}
}

func TestResumeStopAfterOwnershipMoves(t *testing.T) {
	t.Parallel()
	for _, lostReply := range []bool{false, true} {
		t.Run(map[bool]string{false: "restart", true: "lost reply"}[lostReply], func(t *testing.T) {
			s, in, native, pods := resumeRecoveryFixture(t)
			config := resumeRecoveryConfig(s, native, pods)
			config.Reporter = &reportSpy{FinishedFunc: func(ctx context.Context, id string, o argojob.Observation) error {
				r, err := s.Run(ctx, id)
				if err != nil {
					return err
				}
				return s.Finish(ctx, stateport.Action{}, stateport.RunUpdate{ID: id, Revision: r.Revision, Status: o.Status, Reason: o.Reason})
			}}
			stop := func() {
				r, err := s.Run(t.Context(), in.RunID)
				compositionOK(t, err)
					_, err = s.Cancel(t.Context(), stateport.Action{Actor: r.Origin.Actor}, stateport.RunCancellation{ID: r.ID, Revision: r.Revision})
				compositionOK(t, err)
			}
			if lostReply {
				config.Workflows = &resumeLostReply{Workflows: native, afterApply: stop}
			} else {
				compositionOK(t, argojob.New(config).ApplyResume(t.Context(), in.RunID))
				stop()
			}
			compositionOK(t, argojob.New(config).ApplyResume(t.Context(), in.RunID))
			if native.Object.Spec.Shutdown != wf.ShutdownStrategyTerminate || native.Posts != 0 {
				t.Fatal("saved stop left resumed Workflow executing", native.Object.Spec.Shutdown, native.Posts)
			}
			r, err := s.Run(t.Context(), in.RunID)
			compositionOK(t, err)
			if r.Status != stepmodel.RunCancelled {
				t.Fatal("saved stop did not cancel successor", r.Status)
			}
		})
	}
}

type resumeLostReply struct {
	*fake.Workflows
	afterApply func()
}

func (f *resumeLostReply) Patch(ctx context.Context, q argojob.PatchRequest) (*wf.Workflow, error) {
	w, err := f.Workflows.Patch(ctx, q)
	if err == nil && f.afterApply != nil {
		after := f.afterApply
		f.afterApply = nil
		after()
		return nil, errors.New("patch reply lost")
	}
	return w, err
}

func checkResumeRace(t *testing.T, scenario string) {
	t.Helper()
	s, in, native, pods := resumeRecoveryFixture(t)
	config := resumeRecoveryConfig(s, native, pods)
	config.Reporter = &reportSpy{FinishedFunc: func(ctx context.Context, id string, o argojob.Observation) error {
		r, err := s.Run(ctx, id)
		if err != nil {
			return err
		}
		return s.Finish(ctx, stateport.Action{}, stateport.RunUpdate{ID: id, Revision: r.Revision, Status: o.Status, Reason: o.Reason})
	}}
	stop := func() {
		r, err := s.Run(t.Context(), in.RunID)
		compositionOK(t, err)
		_, err = s.Cancel(t.Context(), stateport.Action{Actor: r.Origin.Actor}, stateport.RunCancellation{ID: r.ID, Revision: r.Revision})
		compositionOK(t, err)
	}
	switch scenario {
	case "changed version":
		native.Object.ResourceVersion = "changed"
	case "replaced Pod":
		pods.objects[1] = pods.objects[1].DeepCopy()
		pods.objects[1].UID = "replacement"
	case "saved stop":
		stop()
	case "stop after delete":
		pods.beforeDelete = stop
	case "terminal successor":
		r, err := s.Run(t.Context(), in.RunID)
		compositionOK(t, err)
		compositionOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunFailed}))
	}
	err := argojob.New(config).ApplyResume(t.Context(), in.RunID)
	if scenario != "saved stop" && err == nil {
		t.Fatal("changed resume applied")
	}
	if native.Patches != 0 || scenario != "stop after delete" && pods.Deletes != 0 {
		t.Fatal("refusal changed native state", native.Patches, pods.Deletes)
	}
}
