package store

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRunResourcesDefaultIncludesOnlyLinkedTargetPromotions(t *testing.T) {
	now := time.Now().UTC()
	s := testStore(t, &now)
	repo := createRepo(t, s)
	direct, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "main", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{direct.ID}
	for i, target := range []string{"main", "main/sub", "main"} {
		sha := strings.Repeat(string(rune('b'+i)), 40)
		p, err := s.AppendPromotion(t.Context(), model.PromotionSpec{RepoID: repo.ID, SourceBranch: "step-abc12345", SourceSHA: sha, TargetRef: target, Actor: "repo-01"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.PlanPromotion(t.Context(), p.ID, strings.Repeat("a", 40), sha); err != nil {
			t.Fatal(err)
		}
		run, _, err := s.EnqueueAdmittedPromotionRun(t.Context(), model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "promotion/" + target + "/" + sha[:12], SHA: sha, TestedSHA: sha, BaseSHA: strings.Repeat("a", 40), Actor: "repo-01", Trigger: "promotion"}, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
	}
	spoof, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "promotion/main/step-abc12345", strings.Repeat("e", 40)))
	if err != nil {
		t.Fatal(err)
	}
	ids = append(ids, spoof.ID)
	for i, id := range ids {
		status := "passed"
		if i == 3 {
			status = "failed"
		}
		if _, err := s.db.Exec(`UPDATE runs SET status=? WHERE id=?`, status, id); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.RunResources(t.Context(), repo.ID, RunResourceFilter{Ref: "main", Runs: 30, IncludePromotions: true})
	if err != nil || len(got.Runs) != 2 || got.Runs[0].ID != ids[1] || got.Runs[1].ID != ids[0] {
		t.Fatalf("default selection: %+v %v", got.Runs, err)
	}
	got, err = s.RunResources(t.Context(), repo.ID, RunResourceFilter{Ref: "main", Runs: 30})
	if err != nil || len(got.Runs) != 1 || got.Runs[0].ID != ids[0] {
		t.Fatalf("explicit ref: %+v %v", got.Runs, err)
	}
	got, err = s.RunResources(t.Context(), repo.ID, RunResourceFilter{Ref: "main", Runs: 1, IncludePromotions: true, IncludeFailed: true})
	if err != nil || len(got.Runs) != 1 || got.Runs[0].ID != ids[3] {
		t.Fatalf("combined ordering and bound: %+v %v", got.Runs, err)
	}
}

func TestRunResourcesReadsOwnPeakOnlyForMatchingFinalAttempt(t *testing.T) {
	at := time.Now().UTC()
	s := testStore(t, &at)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "main", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunWorkflowName(t.Context(), run.ID, "step-abc12345"); err != nil {
		t.Fatal(err)
	}
	v := resourceRow(run.ID, at)
	v.WorkflowName = "step-abc12345"
	v.PodName = "step-abc12345-1"
	v.NodeID = v.PodName
	v.Namespace = "oberth"
	v.Stage = "step-12345678"
	v.Step = "step-abc12345"
	v.PodStartedAt = at.Add(-time.Minute)
	if err = s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); err != nil {
		t.Fatal(err)
	}
	c := &model.StepTermination{RunID: run.ID, Namespace: v.Namespace, WorkflowName: v.WorkflowName, WorkflowUID: v.WorkflowUID, NodeID: v.NodeID, PodName: v.PodName, PodUID: v.PodUID, ContainerName: "main", ContainerID: v.ContainerID, Reason: "Error", PodReason: "DeadlineExceeded", ExitCode: 143, Signal: 15, StartedAt: at.Add(-time.Minute), FinishedAt: at}
	seconds := int64(60)
	c.ActiveDeadlineSeconds = &seconds
	peak := int64(1536 << 20)
	if _, err = s.PutStepResult(t.Context(), model.StepResult{RunID: run.ID, Stage: v.Stage, Step: v.Step, Status: model.StepTimedOut, DeclaredSize: "M", MemoryPeakBytes: &peak, Termination: &model.StepDiagnostic{Available: true, ObservedAt: &at, Container: c}}); err != nil {
		t.Fatal(err)
	}
	v.ContainerID = "containerd://" + strings.Repeat("c", 64)
	v.RestartCount = 1
	if err = s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunResources(t.Context(), repo.ID, RunResourceFilter{Ref: "main", Runs: 30, IncludeFailed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Observations) != 2 {
		t.Fatal("missing attempts")
	}
	bound := 0
	for _, o := range got.Observations {
		if o.DeadlineKilled != (o.Usage.RestartCount == 0) {
			t.Fatal("deadline kill crossed incarnation or was lost")
		}
		if o.Usage.RestartCount != 0 && !o.DeadlineUncertain {
			t.Fatal("unbound deadline lost its conservative refusal")
		}
		if o.StepMemoryPeakBytes != nil {
			bound++
			if *o.StepMemoryPeakBytes != peak || o.Usage.RestartCount != 0 {
				t.Fatal("own peak crossed incarnation")
			}
		}
	}
	if bound != 1 {
		t.Fatal("own peak was not bound to exactly one attempt")
	}
}

func TestRunResourcesWorkflowDeadlineCensorsWithoutStepDiagnostic(t *testing.T) {
	at := time.Now().UTC()
	s := testStore(t, &at)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "main", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetRunWorkflowName(t.Context(), run.ID, "step-abc12345"); err != nil {
		t.Fatal(err)
	}
	v := resourceRow(run.ID, at)
	v.WorkflowName = "step-abc12345"
	if err = s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{v}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE runs SET status='failed',error='Workflow deadline exceeded' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunResources(t.Context(), repo.ID, RunResourceFilter{Ref: "main", Runs: 30, IncludeFailed: true})
	if err != nil || len(got.Observations) != 1 || got.Observations[0].DeadlineKilled || !got.Observations[0].DeadlineUncertain {
		t.Fatalf("workflow deadline censoring lost: %+v %v", got.Observations, err)
	}
}
