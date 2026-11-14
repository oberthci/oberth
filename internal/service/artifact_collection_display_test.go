package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func TestArtifactDeadlineReasonPreservesWorkflowVerdict(t *testing.T) {
	for _, status := range []model.RunStatus{model.RunPassed, model.RunFailed} {
		run := model.Run{Status: status, Error: model.ArtifactCollectionDeadlineReason}
		want := model.ArtifactCollectionDeadlineReason
		if status == model.RunFailed {
			run.Phase, run.FailedStep = "test", "unit"
			run.Error = "original step error\n" + model.ArtifactCollectionDeadlineReason
			want = "unit failed · " + want
		}
		got := presentationRun(run)
		if got.DisplayReason != want || got.Status != status {
			t.Fatal(got)
		}
	}
}

func TestArtifactDeadlineSurvivesResourceGuidance(t *testing.T) {
	runID := strings.Repeat("a", 32)
	original := JobResult{Status: model.RunFailed, Phase: "job", FailedStep: "unit", Error: "Workflow deadline exceeded\n" + model.ArtifactCollectionDeadlineReason,
		Steps: []model.StepResult{{RunID: runID, Stage: "test", Step: "unit", Status: model.StepFailed}}}
	result := (&Scheduler{}).withResourceFailure(t.Context(), runID, original)
	if result.Status != model.RunFailed || !strings.HasPrefix(result.Error, model.ExecutionDeadlineMessage+" Step ") || !strings.HasSuffix(result.Error, "\n"+model.ArtifactCollectionDeadlineReason) {
		t.Fatal(result)
	}
	shown := presentationRun(model.Run{Status: result.Status, Error: result.Error, Phase: result.Phase, FailedStep: result.FailedStep})
	if shown.DisplayReason != "unit failed · deadline · "+model.ArtifactCollectionDeadlineReason {
		t.Fatal(shown)
	}
}

func TestArtifactDeadlineDoesNotChangeFailureClassification(t *testing.T) {
	for _, tc := range []struct{ message, reason, cause string }{
		{"Evicted", "evicted by kubelet", "eviction"},
		{"Workflow deadline exceeded", "infrastructure error: deadline exceeded", "deadline"},
	} {
		t.Run(tc.message, func(t *testing.T) {
			run := model.Run{Status: model.RunFailed, Phase: "infrastructure", Error: tc.message + "\n" + model.ArtifactCollectionDeadlineReason}
			shown := presentationRun(run)
			if shown.Error != run.Error || shown.DisplayReason != tc.reason+" · "+model.ArtifactCollectionDeadlineReason || strings.Count(shown.DisplayReason, model.ArtifactCollectionDeadlineReason) != 1 {
				t.Fatal(shown)
			}
			if failedRunDisplayReason(run) != tc.reason {
				t.Fatal("classifier included collection evidence", failedRunDisplayReason(run))
			}
			hint := runRetryHint(run, nil)
			if hint == nil || hint.Class != model.RetryInfrastructure || hint.Cause != tc.cause {
				t.Fatal(hint)
			}
		})
	}
}

func TestArtifactDeadlineSurvivesSchedulerPublication(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "delivered", true: "transport-retry"}[retry], func(t *testing.T) {
			f := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "passed", Error: model.ArtifactCollectionDeadlineReason})
			var transport *retryPublicationGit
			if retry {
				transport = &retryPublicationGit{controlGit: f.git, failure: errors.New("connection reset by peer")}
				f.scheduler.git = transport
			}
			run, err := f.scheduler.recordCIForTest(t.Context(), CIRequest{EventID: "collection-deadline", Repository: f.repo, Branch: "feature/collection", SHA: strings.Repeat("a", 40), Actor: "agent@host"})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.scheduler.ProcessNext(t.Context()); err != nil {
				t.Fatal(err)
			}
			if retry {
				pending, err := f.store.PendingPublications(t.Context())
				if err != nil || len(pending) != 1 {
					t.Fatal(pending, err)
				}
				current, err := f.store.Run(t.Context(), run.ID)
				if err != nil || !strings.HasSuffix(current.Error, "\n"+model.ArtifactCollectionDeadlineReason) {
					t.Fatal(current, err)
				}
				pub := pending[0]
				if _, err = f.store.UpdatePublicationAttempt(t.Context(), pub.ID, pub.Attempts, pub.Attempts, time.Now().Add(-time.Second), pub.Error); err != nil {
					t.Fatal(err)
				}
				transport.failure = nil
				if err := f.scheduler.recoverPendingPublicationsWithGate(t.Context(), f.scheduler.requireMutation); err != nil {
					t.Fatal(err)
				}
			}
			current, err := f.store.Run(t.Context(), run.ID)
			if err != nil || current.Status != model.RunPassed || current.Error != model.ArtifactCollectionDeadlineReason || current.Assisted {
				t.Fatal(current, err)
			}
			shown := presentationRun(current)
			if shown.DisplayReason != model.ArtifactCollectionDeadlineReason {
				t.Fatal(shown)
			}
			if _, err := f.store.OpenCIIssue(t.Context(), f.repo.ID, current.Ref); !errors.Is(err, store.ErrNotFound) {
				t.Fatal(err)
			}
			if pending, err := f.store.PendingPublications(t.Context()); err != nil || len(pending) != 0 {
				t.Fatal(pending, err)
			}
		})
	}
}
