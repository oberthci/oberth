package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type waitingRestartJobs struct {
	recoveryJobs
	terminalReads int
}

func (jobs *waitingRestartJobs) CreateRelease(ctx context.Context, request WorkflowRequest) error {
	return jobs.CreateWorkflow(ctx, request)
}

func (jobs *waitingRestartJobs) TerminalResult(ctx context.Context, name string) (JobResult, error) {
	jobs.terminalReads++
	return jobs.recoveryJobs.TerminalResult(ctx, name)
}

func TestClaimedWaitingRunsAreNeverResubmittedAfterRestart(t *testing.T) {
	for _, kind := range []string{"branch", "tag", "release", "promotion"} {
		t.Run(kind, func(t *testing.T) {
			f := newRecoveryFixture(t)
			ctx := t.Context()
			spec := model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "feature/waiting",
				SHA: strings.Repeat("a", 40), Actor: "tester", Trigger: "branch"}
			var enqueued model.EnqueueRunResult
			var promotion model.Promotion
			var err error
			if kind == "promotion" {
				spec.Trigger, spec.Ref, spec.BaseSHA = kind, "promotion/main/aaaaaaaaaaaa", strings.Repeat("b", 40)
				enqueued, promotion, err = storetest.EnqueuePromotionRun(f.store, ctx, spec, model.PromotionSpec{
					RepoID: f.repo.ID, SourceBranch: "feature/promote", SourceSHA: spec.SHA, TargetRef: "main",
					PreviousSHA: spec.BaseSHA, ResultSHA: spec.SHA, Actor: spec.Actor})
			} else {
				if kind != "branch" {
					spec.RefKind, spec.Ref, spec.Trigger, spec.Release = model.RefTag, "v1", kind, kind == "release"
				}
				enqueued, err = storetest.EnqueueRun(f.store, ctx, spec)
			}
			if err != nil {
				t.Fatal(err)
			}
			run, err := f.store.ClaimNextRun(ctx)
			if err != nil || run.ID != enqueued.ID {
				t.Fatalf("claim: %+v %v", run, err)
			}
			f.setWorkflowName(t, run.ID, "workflow-before-admission")
			jobs := &waitingRestartJobs{recoveryJobs: recoveryJobs{terminalErr: ErrJobNotTerminal,
				waitResult: JobResult{Status: model.RunFailed, Phase: "job", Error: "CI completed after restart",
					Steps: []model.StepResult{stepResult(model.StepFailed)}}}}
			scheduler := f.scheduler(t, restartStateJobs{JobController: jobs})
			scheduler.releaseJobs = jobs
			source := filepath.Join(scheduler.workspaceRoot, run.ID, "src")
			if err = os.MkdirAll(source, 0700); err != nil {
				t.Fatal(err)
			}
			log, err := scheduler.logs.Create(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = log.WriteString("waiting before restart\n"); err != nil {
				t.Fatal(err)
			}
			if err = log.Close(); err != nil {
				t.Fatal(err)
			}
			if err = f.store.RecoverOwnerState(ctx); err != nil {
				t.Fatal(err)
			}
			if err = scheduler.reconcileStrandedRunsWithGate(ctx, scheduler.requireMutation, nil); err != nil {
				t.Fatal(err)
			}
			if jobs.terminalReads != 0 || len(jobs.deleted) != 0 {
				t.Fatalf("unadmitted work was reconciled as a submitted Workflow: reads=%d deleted=%v", jobs.terminalReads, jobs.deleted)
			}
			if err = scheduler.ProcessNext(ctx); !errors.Is(err, store.ErrNotFound) {
				t.Fatal(err)
			}
			current, err := f.store.Run(ctx, run.ID)
			if err != nil || current.Status != model.RunFailed || current.Error != model.ServerRestartReason {
				t.Fatalf("waiting run did not execute under its original ID: %+v %v", current, err)
			}
			retained, err := scheduler.logs.Tail(run.ID, 1024)
			if err != nil || !strings.Contains(string(retained), "waiting before restart") {
				t.Fatalf("waiting log lost: %q %v", retained, err)
			}
			if promotion.ID != "" {
				current, err := f.store.Promotion(ctx, promotion.ID)
				if err != nil || current.Status != model.PromotionFailed || current.RunID != run.ID {
					t.Fatalf("promotion did not follow its original CI run: %+v %v", current, err)
				}
			}
		})
	}
}
