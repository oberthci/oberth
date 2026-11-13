package argojob

import (
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/pkg/pipeline"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestOperatorShutdownKeepsArgoReasonThroughDiagnostics(t *testing.T) {
	for _, strategy := range []wfv1.ShutdownStrategy{wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate} {
		t.Run(string(strategy), func(t *testing.T) {
			wf := nestedWorkflow()
			wf.Spec.Shutdown = strategy
			wf.Status.Phase = wfv1.WorkflowFailed
			wf.Status.Message = "Stopped with strategy '" + string(strategy) + "'"
			node := wf.Status.Nodes["prepare"]
			node.Phase, node.Message = wfv1.NodeFailed, "Evicted: The node was low on resource: memory."
			wf.Status.Nodes["prepare"] = node
			got := completionFor(wf)
			for i := range got.Steps {
				got.Steps[i].restartCutReason = "lost exit during restart"
			}
			got.classifyInfrastructure()
			got.classifyRestartCut()
			if !got.OperatorStopped || got.Infrastructure || got.Interrupted || got.Reason != wf.Status.Message {
				t.Fatalf("operator intent replaced by diagnostics: %+v", got)
			}
			wf.Status.Phase = wfv1.WorkflowRunning
			if completionFor(wf).OperatorStopped {
				t.Fatal("shutdown request manufactured terminal outcome")
			}
		})
	}
}

// Argo preserves the exit handler's failure when the main node succeeds, even
// when shutdown intent is present. Neither that failure nor a near-match message
// establishes that the controller stopped the Workflow.
func TestOperatorShutdownRequiresTerminalAttribution(t *testing.T) {
	for _, strategy := range []wfv1.ShutdownStrategy{wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate} {
		for _, message := range []string{"main failed: exit code 23", "exit handler failed", "", "Stopped with strategy 'invalid'", "Stopped with strategy 'Stop': cleanup failed"} {
			t.Run(string(strategy)+"/"+message, func(t *testing.T) {
				wf := nestedWorkflow()
				wf.Spec.Shutdown = strategy
				wf.Status.Phase, wf.Status.Message = wfv1.WorkflowFailed, message
				node := wf.Status.Nodes["lint"]
				node.Phase, node.Message = wfv1.NodeFailed, "exit code 23"
				wf.Status.Nodes["lint"] = node
				got := completionFor(wf)
				if got.OperatorStopped || got.Succeeded || got.FailedStage != "verify" || got.FailedStep != "lint" {
					t.Fatalf("independent failure lost: %+v", got)
				}
			})
		}
		wf := nestedWorkflow()
		wf.Spec.Shutdown = strategy
		other := wfv1.ShutdownStrategyStop
		if strategy == other {
			other = wfv1.ShutdownStrategyTerminate
		}
		wf.Status.Phase = wfv1.WorkflowError
		wf.Status.Message = "Stopped with strategy '" + string(other) + "'"
		if got := completionFor(wf); !got.OperatorStopped || got.Reason != wf.Status.Message {
			t.Fatalf("completed shutdown lost after strategy change: %+v", got)
		}
		wf.Spec.OnExit = "cleanup"
		wf.Status.Message = "exit handler failed"
		wf.Status.Nodes["cleanup"] = wfv1.NodeStatus{ID: "cleanup", Name: "wf.onExit", TemplateName: "cleanup", Type: wfv1.NodeTypePod, Phase: wfv1.NodeFailed, Message: wf.Status.Message}
		if got := completionFor(wf); got.OperatorStopped || got.Reason != "exit handler failed" {
			t.Fatalf("exit failure replaced: %+v", got)
		}
	}
}

func TestCreateSubmitsAuthoredShutdownUnchanged(t *testing.T) {
	for _, strategy := range []wfv1.ShutdownStrategy{wfv1.ShutdownStrategyStop, wfv1.ShutdownStrategyTerminate} {
		t.Run(string(strategy), func(t *testing.T) {
			config := seedTestConfig()
			kube := fake.NewClientset()
			runningSeedPods(kube)
			client := &createRetryWorkflowClient{}
			controller, err := NewController(client, kube, config)
			if err != nil {
				t.Fatal(err)
			}
			var seeds []seedExecCall
			controller.WithSourceSeeder(NewSourceSeeder(kube, recordingExec(&seeds), config))
			source := strings.Replace(greedyDocument, "spec:\n", "spec:\n  shutdown: "+string(strategy)+"\n", 1)
			request := testRequest(pipeline.TriggerCI, source)
			request.SourceDir = writeCheckout(t)
			if _, err = controller.Create(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if len(client.posts) != 1 || client.posts[0].Spec.Shutdown != strategy || client.stored.Spec.Shutdown != strategy {
				t.Fatal("production submission changed authored shutdown", client.posts)
			}
		})
	}
}
