package argojob

import (
	"context"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCompletionKubeletMessagesSurvivePodGC(t *testing.T) {
	for _, message := range []string{
		`Usage of EmptyDir volume "var-run-argo" exceeds the limit "128Mi".`,
		`The node was low on resource: ephemeral-storage. Threshold quantity: 1Gi, available: 123Ki.`,
		`The node was low on resource: memory.`,
		`main: OOMKilled (exit code 137)`,
		`wait: OOMKilled (exit code 137): application-controlled termination log`,
	} {
		t.Run(message, func(t *testing.T) {
			wf, node, _, _ := diagnosticFixture()
			node.Message = message
			wf.Status.Nodes[node.ID] = node
			completion := completionFor(wf)
			controller := &Controller{kube: fake.NewClientset()}
			controller.collectStepDiagnostics(context.Background(), wf, &completion)
			if completion.Succeeded || !completion.Infrastructure || !strings.Contains(completion.Reason, strings.SplitN(message, ": application", 2)[0]) || strings.Contains(completion.Reason, "application-controlled") {
				t.Fatalf("lost kubelet reason: %#v", completion)
			}
			if completion.Steps[0].Termination.Unavailable != "observation-unavailable" {
				t.Fatal("invented termination evidence after Pod GC")
			}
		})
	}
}

func TestCompletionExitCodeAndApplicationMessagesAreNotInfrastructure(t *testing.T) {
	for _, message := range []string{"", "main: Error (exit code 137)", "main: Error (exit code 143)", "main: Error (exit code 1): OOMKilled", "main: Error (exit code 1): The node was low on resource: memory."} {
		wf, node, _, _ := diagnosticFixture()
		node.Message = message
		wf.Status.Nodes[node.ID] = node
		if got := completionFor(wf); got.Infrastructure || got.Succeeded {
			t.Fatalf("test failure reclassified: %#v", got)
		}
	}
}

func TestCompletionOwnedPodEvictionAndOOM(t *testing.T) {
	for _, reason := range []string{"Evicted", "OOMKilled"} {
		t.Run(reason, func(t *testing.T) {
			wf, node, pod, _ := diagnosticFixture()
			pod.Status.Phase = corev1.PodFailed
			if reason == "Evicted" {
				pod.Status.Reason = reason
				pod.Status.Message = "kubelet storage pressure detail"
				// Kubelet eviction can happen before a current termination exists.
				pod.Status.ContainerStatuses = nil
			} else {
				pod.Status.ContainerStatuses[0].State.Terminated.Message = "application-controlled"
			}
			controller := &Controller{kube: fake.NewClientset(pod)}
			completion := completionFor(wf)
			controller.collectStepDiagnostics(context.Background(), wf, &completion)
			if !completion.Infrastructure || !strings.Contains(completion.Reason, reason) || strings.Contains(completion.Reason, "application-controlled") {
				t.Fatalf("owned Pod not classified: %#v", completion)
			}
			if reason == "Evicted" && completion.Reason != "Evicted: "+pod.Status.Message {
				t.Fatal("kubelet message lost")
			}
			pod.OwnerReferences[0].UID = "foreign"
			controller.kube = fake.NewClientset(pod)
			completion = completionFor(wf)
			controller.collectStepDiagnostics(context.Background(), wf, &completion)
			if completion.Infrastructure {
				t.Fatal("foreign Pod changed verdict")
			}
			// An independent test failure keeps normal CI-red attribution.
			node.Message = "main: OOMKilled (exit code 137)"
			wf.Status.Nodes[node.ID] = node
			other := node
			other.ID, other.Name, other.DisplayName, other.Message = "other", wf.Name+".unit", "unit", "main: Error (exit code 1)"
			other.StartedAt.Time = node.StartedAt.Add(-time.Second)
			wf.Status.Nodes[other.ID] = other
			if got := completionFor(wf); got.Infrastructure || got.FailedStep != "unit" {
				t.Fatal("eviction hid separate test failure")
			}
			// A successful final retry is green, regardless of an old eviction.
			delete(wf.Status.Nodes, other.ID)
			node.Phase = wfv1.NodeSucceeded
			wf.Status.Nodes[node.ID], wf.Status.Phase = node, wfv1.WorkflowSucceeded
			if got := completionFor(wf); got.Infrastructure || !got.Succeeded {
				t.Fatal("old failure changed successful retry")
			}
		})
	}
}

func TestKubeletMessageIsBounded(t *testing.T) {
	message := boundedKubeletMessage(strings.Repeat("界", 2000))
	if len(message) > 4096 || strings.ContainsRune(message, '\ufffd') {
		t.Fatal("unbounded or broken UTF-8 message")
	}
}
