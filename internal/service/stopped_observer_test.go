package service

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/storetest"
)

type exitTokenJobs struct {
	*fakeJobs
	token     sync.Once
	delivered chan struct{}
	finished  atomic.Bool
}

func (j *exitTokenJobs) Adopt(context.Context, model.Run) (bool, error) { return true, nil }
func (*exitTokenJobs) WorkflowNamespace() string                        { return "pipeline" }
func (j *exitTokenJobs) SettleCreate(ctx context.Context, name, id string, _ time.Time) (bool, error) {
	if err := j.Stop(ctx, name, id); err != nil {
		return false, err
	}
	return j.finished.Load(), nil
}
func (j *exitTokenJobs) WaitResumed(ctx context.Context, _ string, output io.Writer, _ []runprogress.Event, _ int64) (JobResult, error) {
	if err := ctx.Err(); err != nil {
		return JobResult{}, err
	}
	j.token.Do(func() { close(j.delivered) })
	if _, err := io.WriteString(output, "exit handler received its executor token and finished\n"); err != nil {
		return JobResult{}, err
	}
	j.finished.Store(true)
	return JobResult{Status: model.RunFailed, Phase: "job", Error: "Stopped with strategy Stop", Steps: []model.StepResult{{Stage: "exit", Step: "cleanup", Status: model.StepPassed}}}, nil
}
func (j *exitTokenJobs) TerminalResult(context.Context, string) (JobResult, error) {
	if !j.finished.Load() {
		return JobResult{}, ErrJobNotTerminal
	}
	return JobResult{Status: model.RunFailed, Phase: "job", Error: "Stopped with strategy Stop"}, nil
}

func TestStoppedSettlementResumesNormalObserverForExitToken(t *testing.T) {
	f := newControlFixture(t)
	run, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "stop-exit-token", Repository: f.repo, Branch: f.repo.DefaultBranch, SHA: strings.Repeat("a", 40), Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.store.ClaimNextRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = f.store.SetRunWorkflowName(t.Context(), claimed.ID, "workflow")
	if err != nil {
		t.Fatal(err)
	}
	jobs := &exitTokenJobs{fakeJobs: f.jobs, delivered: make(chan struct{})}
	f.scheduler.jobs = jobs
	if err := f.store.MarkRunCreatePending(t.Context(), claimed.ID, claimed.WorkflowName); err != nil {
		t.Fatal(err)
	}
	f.scheduler.registerCreateCleanup(claimed, true)
	f.scheduler.settleOneCreate(t.Context())
	select {
	case <-jobs.delivered:
	case <-time.After(testStartTimeout(2 * time.Second)):
		t.Fatal("accepted Stop left onExit without the normal token-delivering observer")
	}
	waitForFixtureRunStatus(t, f, run.ID, model.RunInterrupted)
	f.scheduler.createObservers.Wait()
	f.scheduler.createMu.Lock()
	f.scheduler.createPending[run.ID].next = time.Time{}
	f.scheduler.createMu.Unlock()
	f.scheduler.settleOneCreate(t.Context())
	final, err := f.store.Run(t.Context(), run.ID)
	if err != nil || final.Status != model.RunInterrupted || final.Phase != model.PhaseCancelled || f.scheduler.pendingCreateCount() != 0 {
		t.Fatalf("Stop observer did not settle: %+v %v", final, err)
	}
	steps, err := f.store.StepResults(t.Context(), run.ID)
	if err != nil || len(steps) != 1 || steps[0].Step != "cleanup" || steps[0].Status != model.StepPassed {
		t.Fatalf("onExit step evidence lost: %+v %v", steps, err)
	}
	body, _, _, err := f.logs.ReadFrom(run.ID, 0, 1<<20)
	if err != nil || !strings.Contains(string(body), "exit handler received its executor token") {
		t.Fatalf("onExit output lost: %q %v", body, err)
	}
}

func TestStopRacedSucceededPromotionUsesNormalPublication(t *testing.T) {
	f := newControlFixture(t)
	previous, result := strings.Repeat("a", 40), strings.Repeat("b", 40)
	run, promotion, err := storetest.EnqueuePromotionRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/candidate", SHA: result, Actor: "owner", Trigger: "promotion", TestedSHA: result, BaseSHA: previous}, model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/source", SourceSHA: result, TargetRef: f.repo.DefaultBranch, PreviousSHA: previous, ResultSHA: result, Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.store.ClaimNextRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = f.store.SetRunWorkflowName(t.Context(), claimed.ID, "succeeded-workflow")
	if err != nil {
		t.Fatal(err)
	}
	f.git.remoteRefs = map[string]string{"refs/heads/" + f.repo.DefaultBranch: previous}
	f.jobs.stopResult = JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{{Stage: "ci", Step: "tolerated", Status: model.StepFailed, ExitCode: 42}}}
	if err := f.store.MarkRunCreatePending(t.Context(), run.ID, claimed.WorkflowName); err != nil {
		t.Fatal(err)
	}
	f.scheduler.registerCreateCleanup(claimed, true)
	f.scheduler.settleOneCreate(t.Context())
	final, err := f.store.Run(t.Context(), run.ID)
	if err != nil || final.Status != model.RunPassed || f.scheduler.pendingCreateCount() != 0 {
		t.Fatalf("succeeded Stop-race did not settle: %+v %v", final, err)
	}
	promoted, err := f.store.Promotion(t.Context(), promotion.ID)
	if err != nil || promoted.Status != model.PromotionPassed {
		t.Fatalf("native success did not promote: %+v %v", promoted, err)
	}
	if got := f.git.remoteRefs["refs/heads/"+f.repo.DefaultBranch]; got != result {
		t.Fatalf("native success was not published: %s", got)
	}
	steps, err := f.store.StepResults(t.Context(), run.ID)
	if err != nil || len(steps) != 1 || steps[0].Status != model.StepFailed {
		t.Fatalf("tolerated step evidence lost: %+v %v", steps, err)
	}
}
