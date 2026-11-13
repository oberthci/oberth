package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestCancelEscalatesToNativeTerminate(t *testing.T) {
	for _, kind := range []string{"branch", "release", "promotion"} {
		t.Run(kind, func(t *testing.T) {
			now := time.Now()
			s := testStore(t, &now)
			repo := createRepo(t, s)
			spec := testRunSpec(repo.ID, "feature/stop", strings.Repeat("a", 40))
			if kind == "release" {
				spec.Release = true
				spec.RefKind = model.RefTag
				spec.Trigger = "tag"
				spec.Ref = "v1.0.0"
			}
			if kind == "promotion" {
				spec.Trigger = "promotion"
			}
			queued, err := s.EnqueueRun(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ClaimNextRun(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err = s.SetRunWorkflowName(t.Context(), queued.ID, "workflow"); err != nil {
				t.Fatal(err)
			}
			first, err := s.CancelRun(t.Context(), model.RunCancelRequest{RunID: queued.ID, Actor: "operator", Force: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(first.Cancellations) != 1 || first.Cancellations[0].Terminate != (kind != "branch") {
				t.Fatalf("%s cancellation native strategy=%+v", kind, first)
			}
			now = now.Add(time.Second)
			second, err := s.CancelRun(t.Context(), model.RunCancelRequest{RunID: queued.ID, Actor: "operator", Force: true})
			if err != nil {
				t.Fatal(err)
			}
			pending, err := s.PendingRunCancellations(t.Context())
			if err != nil || len(pending) != 1 || !pending[0].Terminate || len(second.Cancellations) != 1 || !second.Cancellations[0].Terminate || second.Run.Status != model.RunRunning {
				t.Fatalf("second cancel did not durably escalate: %+v %+v %v", second, pending, err)
			}
		})
	}
}

func TestNodeRebootInfrastructureDoesNotOpenCIIssue(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, repo.DefaultBranch, strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	final, err := s.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunFailed, Phase: "infrastructure", Error: model.NodeRebootReasonPrefix + "node worker restarted"})
	if err != nil || final.Status != model.RunFailed || final.Phase != "infrastructure" {
		t.Fatal(final, err)
	}
	if issue, err := s.OpenCIIssue(t.Context(), repo.ID, repo.DefaultBranch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("node reboot opened CI issue: %+v %v", issue, err)
	}
}

func TestCancelledSucceededPromotionAcceptsAlreadyTerminalPromotion(t *testing.T) {
	now := time.Now()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	previous, result := strings.Repeat("a", 40), strings.Repeat("b", 40)
	run, promotion, err := s.EnqueuePromotionRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/candidate", SHA: result, Actor: "owner", Trigger: "promotion", TestedSHA: result, BaseSHA: previous}, model.PromotionSpec{RepoID: repo.ID, SourceBranch: "feature/source", SourceSHA: result, TargetRef: repo.DefaultBranch, PreviousSHA: previous, ResultSHA: result, Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimNextRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CancelRun(t.Context(), model.RunCancelRequest{RunID: run.ID, Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinishPromotion(t.Context(), promotion.ID, model.PromotionFailed, run.ID, "cancelled by previous owner"); err != nil {
		t.Fatal(err)
	}
	final, err := s.FinishRun(t.Context(), run.ID, model.RunResult{Status: model.RunPassed, Phase: "passed"})
	if err != nil || final.Status != model.RunPassed {
		t.Fatalf("Stop-raced success wedged CI: %+v %v", final, err)
	}
	kept, err := s.Promotion(t.Context(), promotion.ID)
	if err != nil || kept.Status != model.PromotionFailed {
		t.Fatalf("cancelled promotion resurrected: %+v %v", kept, err)
	}
}
