package argojob

import (
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCompletionKeepsFailureBeforeCleanup(t *testing.T) {
	for _, cleanupPhase := range []wfv1.NodePhase{wfv1.NodeSucceeded, wfv1.NodeFailed, wfv1.NodeError} {
		t.Run(string(cleanupPhase), func(t *testing.T) {
			workflow := &wfv1.Workflow{}
			workflow.Name = "wf"
			workflow.Status.Phase = wfv1.WorkflowFailed
			workflow.Status.Nodes = wfv1.Nodes{
				"root":    {ID: "root", Name: "wf", Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeFailed},
				"test":    {ID: "test", Name: "wf.test", DisplayName: "test", BoundaryID: "root", Type: wfv1.NodeTypePod, Phase: wfv1.NodeFailed, StartedAt: metav1.NewTime(time.Unix(1000, 0)), FinishedAt: metav1.NewTime(time.Unix(1010, 0))},
				"cleanup": {ID: "cleanup", Name: "wf.cleanup", DisplayName: "cleanup", BoundaryID: "root", Type: wfv1.NodeTypePod, Phase: cleanupPhase, StartedAt: metav1.NewTime(time.Unix(1011, 0)), FinishedAt: metav1.NewTime(time.Unix(1020, 0))},
			}
			got := completionFor(workflow)
			if got.Succeeded || got.FailedStage != "test" || got.FailedStep != "test" {
				t.Fatalf("failure attributed to %s/%s (succeeded=%t), want test/test", got.FailedStage, got.FailedStep, got.Succeeded)
			}
			if len(got.Steps) != 2 || got.Steps[1].Step != "cleanup" {
				t.Fatal("cleanup evidence missing", got.Steps)
			}
		})
	}
}
