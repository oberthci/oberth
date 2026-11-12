package service

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func submittedDisplayRun(t *testing.T, f *controlFixture, ref string) model.Run {
	t.Helper()
	enqueued, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: ref, Repository: f.repo, Branch: ref, SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.ClaimNextRun(t.Context())
	if err != nil || run.ID != enqueued.ID {
		t.Fatal(run, err)
	}
	if _, err = f.store.SetRunWorkflowName(t.Context(), run.ID, "workflow-"+run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = f.store.MarkRunSubmitted(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestFirstRunListReadReportsNativeQueueAndSuspensionWithoutStepHistory(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprint(scoped), func(t *testing.T) {
			f := newControlFixture(t)
			queued := submittedDisplayRun(t, f, "queue")
			suspended := submittedDisplayRun(t, f, "suspended")
			s := f.api(t)
			var resumed atomic.Bool
			s.schedulingObserver = schedulingObserverFunc(func(_ context.Context, run model.Run) (*model.ExecutionObservation, error) {
				observation := &model.ExecutionObservation{State: "observed", Phase: "Running", Message: "Workflow execution is running."}
				if !resumed.Load() {
					if run.ID == queued.ID {
						observation.Phase = "Pending"
						observation.Message = model.ParallelismWaitMessage
					} else {
						observation.Reason = "Suspended"
						observation.Message = model.WorkflowSuspendedMessage
					}
				}
				return observation, nil
			})
			actor := api.Actor{Identity: "owner"}
			if scoped {
				actor.Scope = []string{"codeberg"}
			}
			value, err := s.Runs(t.Context(), actor, api.RunFilter{})
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range value.([]model.Run) {
				if run.ID == queued.ID && (run.DisplayStatus != model.DisplayQueued || !strings.Contains(run.DisplayReason, "Argo workflow slot")) {
					t.Fatal("first response missed Argo queue", run)
				}
				if run.ID == suspended.ID && (run.DisplayStatus != model.DisplayRunning || run.DisplayReason != model.WorkflowSuspendedMessage) {
					t.Fatal("first response missed suspend", run)
				}
				steps, err := f.store.StepResults(t.Context(), run.ID)
				if err != nil || len(steps) != 0 {
					t.Fatal("fixture unexpectedly has local progress", steps, err)
				}
			}
			detail, err := s.Run(t.Context(), actor, suspended.ID)
			if err != nil || detail.(RunDetailResponse).Run.DisplayReason != model.WorkflowSuspendedMessage {
				t.Fatal("detail missed suspension", detail, err)
			}
			resumed.Store(true)
			value, err = s.Runs(t.Context(), actor, api.RunFilter{})
			if err != nil {
				t.Fatal(err)
			}
			for _, run := range value.([]model.Run) {
				if run.DisplayStatus != model.DisplayRunning || strings.Contains(strings.ToLower(run.DisplayReason), "suspend") || strings.Contains(run.DisplayReason, "workflow slot") {
					t.Fatal("native state stayed cached after resume", run)
				}
				durable, err := f.store.Run(t.Context(), run.ID)
				if err != nil || durable.Status != model.RunRunning {
					t.Fatal("display changed durable status", durable, err)
				}
			}
		})
	}
}

func TestRunListNativeObservationHasOnePageBudget(t *testing.T) {
	f := newControlFixture(t)
	for i := range 12 {
		submittedDisplayRun(t, f, fmt.Sprint(i))
	}
	s := f.api(t)
	s.schedulingObserver = schedulingObserverFunc(func(ctx context.Context, _ model.Run) (*model.ExecutionObservation, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	start := time.Now()
	value, err := s.Runs(t.Context(), api.Actor{Identity: "owner"}, api.RunFilter{})
	elapsed := time.Since(start)
	if err != nil || elapsed > 500*time.Millisecond {
		t.Fatal("list waited for per-row Kubernetes deadlines", elapsed, err)
	}
	if len(value.([]model.Run)) != 12 {
		t.Fatal("active runs omitted", value)
	}
	for _, run := range value.([]model.Run) {
		if run.DisplayReason != "awaiting Argo state" {
			t.Fatal("unobserved native state invented", run)
		}
	}
}

func TestSeedPendingReasonPrecedesArgoStateOnAllReadSurfaces(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		t.Run(fmt.Sprint(scoped), func(t *testing.T) {
			f := newControlFixture(t)
			run := submittedDisplayRun(t, f, "source-preparation")
			s := f.api(t)
			const message = "source seed pod pending: ImagePullBackOff"
			s.schedulingObserver = schedulingObserverFunc(func(context.Context, model.Run) (*model.ExecutionObservation, error) {
				return &model.ExecutionObservation{State: "observed", Phase: "Pending", Reason: "SourceSeedPending", Message: message, Pods: []model.SchedulingReason{{Phase: "Pending", Reason: "ImagePullBackOff", Message: "pod pending: container is waiting to start"}}}, nil
			})
			actor := api.Actor{Identity: "owner"}
			if scoped {
				actor.Scope = []string{"codeberg"}
			}
			listed, err := s.Runs(t.Context(), actor, api.RunFilter{})
			if err != nil {
				t.Fatal(err)
			}
			rows := listed.([]model.Run)
			if len(rows) != 1 || rows[0].DisplayStatus != model.DisplayQueued || rows[0].DisplayReason != message {
				t.Fatal("list waited for nonexistent Argo Workflow", rows)
			}
			value, err := s.Run(t.Context(), actor, run.ID)
			if err != nil || value.(RunDetailResponse).Run.DisplayReason != message {
				t.Fatal("run detail hid source reason", value, err)
			}
			scopedAPI, err := s.forActor(t.Context(), actor)
			if err != nil {
				t.Fatal(err)
			}
			status, err := scopedAPI.status(t.Context(), f.repo.Name, run.SHA, "")
			if err != nil || status.Run.DisplayReason != message {
				t.Fatal("status hid source reason", status, err)
			}
			kept, err := f.store.Run(t.Context(), run.ID)
			if err != nil || kept.Status != model.RunRunning || kept.Error != "" {
				t.Fatal("observation changed lifecycle", kept, err)
			}
		})
	}
}
