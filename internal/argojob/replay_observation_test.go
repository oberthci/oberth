package argojob

import (
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func replayTerminalWorkflow() *wfv1.Workflow {
	now := metav1.NewTime(time.Now())
	return &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "observed"}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowFailed, FinishedAt: now, Nodes: wfv1.Nodes{
		"root": {ID: "root", Name: "observed", Type: wfv1.NodeTypeDAG, Phase: wfv1.NodeFailed, Children: []string{"push"}},
		"push": {ID: "push", Name: "observed.push", DisplayName: "push", Type: wfv1.NodeTypePod, Phase: wfv1.NodeFailed, BoundaryID: "root", StartedAt: now, FinishedAt: now},
	}}}
}
func TestReplayTerminalObservationRequiresCompleteTree(t *testing.T) {
	for _, kind := range []string{"complete", "zero nodes", "running workflow", "running node", "missing child", "missing boundary", "missing root", "compressed", "offloaded", "no finished time"} {
		t.Run(kind, func(t *testing.T) {
			wf := replayTerminalWorkflow()
			switch kind {
			case "zero nodes":
				wf.Status.Nodes = nil
			case "running workflow":
				wf.Status.Phase = wfv1.WorkflowRunning
			case "running node":
				node := wf.Status.Nodes["push"]
				node.Phase = wfv1.NodeRunning
				wf.Status.Nodes["push"] = node
			case "missing child":
				delete(wf.Status.Nodes, "push")
			case "missing boundary":
				node := wf.Status.Nodes["push"]
				node.BoundaryID = "lost"
				wf.Status.Nodes["push"] = node
			case "missing root":
				delete(wf.Status.Nodes, "root")
			case "compressed":
				wf.Status.CompressedNodes = "compressed"
			case "offloaded":
				wf.Status.OffloadNodeStatusVersion = "offloaded"
			case "no finished time":
				wf.Status.FinishedAt = metav1.Time{}
			}
			want := kind == "complete" || kind == "zero nodes"
			result := completionFor(wf)
			if result.ObservationComplete != want {
				t.Fatalf("complete=%v want %v", result.ObservationComplete, want)
			}
			if kind == "complete" && (len(result.ReplayAttempts) != 1 || result.ReplayAttempts[0].StartedAt == nil) {
				t.Fatal("proof omitted actual step attempt")
			}
		})
	}
}
