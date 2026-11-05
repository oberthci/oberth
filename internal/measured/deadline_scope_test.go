package measured

import (
	"github.com/oberthci/oberth/internal/model"
	"strings"
	"testing"
	"time"
)

func TestDeadlineUncertaintyRefusesAdviceWithoutInventingKill(t *testing.T) {
	for _, location := range []string{"displayed", "hidden", "duplicate"} {
		t.Run(location, func(t *testing.T) {
			rows := observations()
			assertCoveredAdvice(t, rows)
			uncertain := rows[0]
			uncertain.DeadlineUncertain = true
			var steps []Step
			switch location {
			case "displayed":
				rows[0] = uncertain
				steps = Build(rows, nil)
			case "duplicate":
				steps = Build(append(rows, uncertain), nil)
			case "hidden":
				steps = BuildWithSafety(rows, nil, []Observation{uncertain}, nil)
			}
			s := steps[0]
			if s.Suggested != nil || !s.Flags.DeadlineUnknown || s.Flags.DeadlineKilled || s.SizingAcceptance.CensoredAttempts != 0 || s.Attempts != 5 {
				t.Fatalf("uncertainty became a kill or escaped safety: %+v", s)
			}
			assertNoDeadlineAdvice(t, s, false)
		})
	}
}

func TestCompletedMainIsNotKilledByPodFinalizationDeadline(t *testing.T) {
	at := time.Now().UTC()
	v := observations()[0].Usage
	v.RunID = strings.Repeat("a", 32)
	v.WorkflowUID = "11111111-1111-4111-8111-111111111111"
	v.PodUID = "22222222-2222-4222-8222-222222222222"
	v.ContainerID = "containerd://" + strings.Repeat("b", 64)
	c := &model.StepTermination{RunID: v.RunID, WorkflowUID: v.WorkflowUID, Namespace: "oberth", WorkflowName: "wf", NodeID: "wf-1", PodName: "wf-1", PodUID: v.PodUID, ContainerName: "main", ContainerID: v.ContainerID, Reason: "Completed", PodReason: "DeadlineExceeded", ExitCode: 0, StartedAt: at.Add(-time.Minute), FinishedAt: at}
	d := &model.StepDiagnostic{Available: true, ObservedAt: &at, Container: c}
	got := WithStepResult(Observation{Usage: v}, d, nil, model.StepTimedOut)
	if got.DeadlineKilled || !got.DeadlineUncertain {
		t.Fatalf("completed main mislabeled: %+v", got)
	}
	disposition := model.PodResourceDisposition{RunID: v.RunID, WorkflowName: "wf", WorkflowUID: v.WorkflowUID, Namespace: "oberth", NodeID: "wf-1", PodName: "wf-1", PodUID: v.PodUID, Stage: v.Stage, Step: v.Step, Template: "step-abc12345", Owner: "repo-01", Reason: "DeadlineExceeded", ObservedAt: at}
	s := BuildWithSafety(nil, nil, nil, []model.PodResourceDisposition{disposition})[0]
	if s.Flags.DeadlineKilled || !s.Flags.DeadlineUnknown || s.Suggested != nil || s.SizingAcceptance.CensoredAttempts != 1 {
		t.Fatalf("Pod censor lost or became main kill: %+v", s)
	}
}
