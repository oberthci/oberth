package argojob

import (
	"encoding/json"
	"io"
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofile "github.com/argoproj/argo-workflows/v4/util/file"
	"k8s.io/utils/ptr"
)

func compressCompletionFixture(t *testing.T, workflow *wfv1.Workflow) *wfv1.Workflow {
	t.Helper()
	compressed := workflow.DeepCopy()
	body, err := json.Marshal(compressed.Status.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	compressed.Status.CompressedNodes = argofile.CompressEncodeString(t.Context(), string(body))
	compressed.Status.Nodes = nil
	return compressed
}

func nativeCompletionFixture() *wfv1.Workflow {
	workflow := replayTerminalWorkflow()
	workflow.Namespace, workflow.UID = "pipeline", "workflow-uid"
	workflow.Annotations = map[string]string{runIDAnnotation: "run"}
	node := workflow.Status.Nodes["push"]
	node.Outputs = &wfv1.Outputs{ExitCode: ptr.To("42")}
	workflow.Status.Nodes[node.ID] = node
	return workflow
}

func TestNativeCompressedCompletionUsesOneSnapshot(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		workflow := nativeCompletionFixture()
		if compressed {
			workflow = compressCompletionFixture(t, workflow)
		}
		before := workflow.DeepCopy()
		got := completionFor(workflow)
		if got.FailedNodeID != "push" || len(got.Steps) != 1 || got.Steps[0].ExitCode != 42 || !got.Steps[0].ExitCodeKnown || len(got.NodeSteps) != 2 || got.ObservedNodes != 2 || len(got.ObservedNodeIDs) != 2 || !got.ObservationComplete {
			t.Fatalf("compressed=%v completion lost native evidence: %+v", compressed, got)
		}
		if !reflect.DeepEqual(workflow, before) {
			t.Fatal("completion mutated cached status")
		}
	}
	for _, unavailable := range []string{"malformed", "offloaded", "offloaded-with-nodes"} {
		workflow := nativeCompletionFixture()
		if unavailable == "malformed" {
			workflow.Status.CompressedNodes = "invalid"
		} else {
			workflow.Status.OffloadNodeStatusVersion = "outside"
		}
		if unavailable == "offloaded" {
			workflow.Status.Nodes = nil
		}
		if got := completionFor(workflow); got.ObservationComplete {
			t.Fatal("unavailable status granted replay evidence", unavailable, got)
		}
	}
}

func TestNativeFinalReadsExpandCompressedStatus(t *testing.T) {
	for _, path := range []string{"wait", "terminal", "confirm", "settle"} {
		t.Run(path, func(t *testing.T) {
			plain := nativeCompletionFixture()
			compressed := compressCompletionFixture(t, plain)
			before := compressed.DeepCopy()
			controller := &Controller{workflows: &staticWorkflowClient{workflow: compressed}, config: Config{Namespace: plain.Namespace}, hub: containerLogCache{workflow: compressed}}
			switch path {
			case "wait":
				got, err := controller.Wait(t.Context(), plain.Name, "run", io.Discard)
				if err != nil || got.FailedNodeID != "push" || !got.ObservationComplete {
					t.Fatal(got, err)
				}
			case "terminal":
				got, err := controller.TerminalState(t.Context(), plain.Name)
				if err != nil || got.FailedNodeID != "push" || !got.ObservationComplete {
					t.Fatal(got, err)
				}
			case "confirm":
				got, complete, err := controller.confirmWatchedCompletion(t.Context(), plain, "run", Adoption{})
				if err != nil || !complete || got.Status.CompressedNodes != "" || len(got.Status.Nodes) != 2 {
					t.Fatal(got, complete, err)
				}
			case "settle":
				got, complete := controller.settle(t.Context(), plain.Name, "run", plain)
				if !complete || got.Status.CompressedNodes != "" || len(got.Status.Nodes) != 2 {
					t.Fatal(got, complete)
				}
			}
			if !reflect.DeepEqual(compressed, before) {
				t.Fatal("final read mutated cached status")
			}
		})
	}
}
