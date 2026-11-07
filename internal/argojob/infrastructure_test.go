package argojob

import (
	"context"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCompletionKubeletMessagesSurvivePodGC(t *testing.T) {
	for _, message := range []string{
		`Usage of EmptyDir volume "var-run-argo" exceeds the limit "128Mi".`,
		`The node was low on resource: ephemeral-storage. Threshold quantity: 1Gi, available: 123Ki.`,
		`The node was low on resource: memory.`,
		`Pod ephemeral local storage usage exceeds the total limit of containers 512Mi.`,
		`Evicted: Pod ephemeral local storage usage exceeds the total limit of containers 512Mi.`,
	} {
		t.Run(message, func(t *testing.T) {
			wf, node, _, _ := diagnosticFixture()
			node.Message = message
			wf.Status.Nodes[node.ID] = node
			completion := completionFor(wf)
			controller := &Controller{kube: fake.NewClientset()}
			controller.collectStepDiagnostics(context.Background(), wf, &completion)
			if completion.Succeeded || !completion.Infrastructure || !strings.Contains(completion.Reason, message) || strings.Contains(completion.Reason, "application-controlled") {
				t.Fatalf("lost kubelet reason: %#v", completion)
			}
			if completion.Steps[0].Termination.Unavailable != "pod-absent" {
				t.Fatal("invented termination evidence after Pod GC")
			}
		})
	}
}

func TestCompletionExitCodeAndApplicationMessagesAreNotInfrastructure(t *testing.T) {
	for _, message := range []string{"", "main: OOMKilled (exit code 137)", "wait: OOMKilled (exit code 137): application-controlled termination log", "main: OOMKilled (exit code 137): The node was low on resource: memory.", "main: Error (exit code 137)", "main: Error (exit code 143)", "main: Error (exit code 1): OOMKilled", "main: Error (exit code 1): The node was low on resource: memory."} {
		wf, node, _, _ := diagnosticFixture()
		node.Message = message
		wf.Status.Nodes[node.ID] = node
		if got := completionFor(wf); got.Infrastructure || got.Succeeded {
			t.Fatalf("test failure reclassified: %#v", got)
		}
	}
}

func TestCompletionOwnedPodEvictionAndOOM(t *testing.T) {
	for _, reason := range []string{"Evicted", "node memory pressure"} {
		t.Run(reason, func(t *testing.T) {
			wf, node, pod, _ := diagnosticFixture()
			pod.Status.Phase = corev1.PodFailed
			if reason == "Evicted" {
				pod.Status.Reason = reason
				pod.Status.Message = "kubelet storage pressure detail"
				// Kubelet eviction can happen before a current termination exists.
				pod.Status.ContainerStatuses = nil
			} else {
				pod.Status.Message = "The node was low on resource: memory. Threshold quantity: 100Mi, available: 50Mi."
				pod.Status.ContainerStatuses[0].State.Terminated.Message = "application-controlled"
			}
			controller := &Controller{kube: fake.NewClientset(pod)}
			completion := completionFor(wf)
			controller.collectStepDiagnostics(context.Background(), wf, &completion)
			if !completion.Infrastructure || !strings.Contains(completion.Reason, "Evicted") || strings.Contains(completion.Reason, "application-controlled") {
				t.Fatalf("owned Pod not classified: %#v", completion)
			}
			if len(completion.ReplayAttempts) != 1 || completion.ReplayAttempts[0].InfrastructureReason != completion.Reason {
				t.Fatal("owned pod eviction missing from replay evidence", completion.ReplayAttempts)
			}
			if completion.Reason != "Evicted: "+pod.Status.Message {
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
			node.Message = "The node was low on resource: memory."
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

func TestCompletionContainerMemoryLimitOOMIsTestFailure(t *testing.T) {
	wf, node, pod, _ := diagnosticFixture()
	// The fixture has a 1Gi main limit and a current OOMKilled termination.
	pod.Status.Phase = corev1.PodFailed
	node.Message = "main: OOMKilled (exit code 137)"
	wf.Status.Nodes[node.ID] = node
	completion := completionFor(wf)
	controller := &Controller{kube: fake.NewClientset(pod)}
	controller.collectStepDiagnostics(t.Context(), wf, &completion)
	if completion.Infrastructure || completion.Succeeded || completion.FailedStep != "test" {
		t.Fatalf("step memory limit OOM lost test attribution: %#v", completion)
	}
	termination := completion.Steps[0].Termination
	if !termination.Available || termination.Container.Reason != "OOMKilled" || *termination.Container.MemoryLimitBytes != 1<<30 {
		t.Fatalf("OOM evidence lost: %#v", termination)
	}
}

func TestKubeletMessageIsBounded(t *testing.T) {
	message := boundedKubeletMessage(strings.Repeat("界", 2000))
	if len(message) > 4096 || strings.ContainsRune(message, '\ufffd') {
		t.Fatal("unbounded or broken UTF-8 message")
	}
}

// #1491: when the step declared its own ephemeral-storage limit, the kubelet
// Pod total is the step's budget. Exceeding it is a step resource failure that
// a rerun repeats, so it is not infrastructure and is not auto-retried.
func TestDeclaredStorageEvictionIsStepFailure(t *testing.T) {
	const total = "Pod ephemeral local storage usage exceeds the total limit of containers 1152Mi. "
	declared := wfv1.Template{Name: "test-template", Container: &corev1.Container{Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("640Mi")}}}}
	undeclared := wfv1.Template{Name: "test-template", Container: &corev1.Container{}}
	cases := []struct {
		name       string
		policy     string
		template   wfv1.Template
		nodeMsg    string
		podMsg     string
		stepBudget bool
	}{
		{name: "v3/node", policy: helperStoragePolicy, template: undeclared, nodeMsg: total, stepBudget: true},
		{name: "v3/node-prefixed", policy: helperStoragePolicy, template: declared, nodeMsg: "Evicted: " + total, stepBudget: true},
		{name: "v3/pod-only", policy: helperStoragePolicy, template: declared, nodeMsg: "Evicted", podMsg: total, stepBudget: true},
		{name: "v2/declared-template", policy: cappedHelperStoragePolicy, template: declared, nodeMsg: total, stepBudget: true},
		// v2 limited every helper: an undeclared step's 512Mi total was not its
		// own budget. It stays infrastructure with at most one auto-retry.
		{name: "v2/undeclared-template", policy: cappedHelperStoragePolicy, template: undeclared, nodeMsg: total},
		{name: "v2/unresolved-template", policy: cappedHelperStoragePolicy, template: wfv1.Template{Name: "other", Container: declared.Container}, nodeMsg: total},
		{name: "v1/undeclared-pod", policy: legacyHelperStoragePolicy, template: undeclared, nodeMsg: "Evicted", podMsg: total},
		// Node pressure is never the step's budget.
		{name: "v3/node-pressure", policy: helperStoragePolicy, template: declared, nodeMsg: "The node was low on resource: ephemeral-storage. Threshold quantity: 1Gi, available: 123Ki."},
		{name: "v3/pod-node-pressure", policy: helperStoragePolicy, template: declared, nodeMsg: "Evicted", podMsg: "The node was low on resource: ephemeral-storage. "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf, node, pod, _ := diagnosticFixture()
			wf.Annotations[helperStorageAnnotation] = tc.policy
			wf.Spec.Templates = []wfv1.Template{tc.template}
			node.TemplateName = "test-template"
			node.Message = tc.nodeMsg
			wf.Status.Nodes[node.ID] = node
			pod.Name = PodName(wf, node)
			kube := fake.NewClientset()
			if tc.podMsg != "" {
				pod.Status.Phase = corev1.PodFailed
				pod.Status.Reason = "Evicted"
				pod.Status.Message = tc.podMsg
				pod.Status.ContainerStatuses = nil
				kube = fake.NewClientset(pod)
			}
			completion := completionFor(wf)
			controller := &Controller{kube: kube}
			controller.collectStepDiagnostics(context.Background(), wf, &completion)
			if completion.Succeeded {
				t.Fatal("evicted step succeeded")
			}
			if !tc.stepBudget {
				if !completion.Infrastructure || completion.ReplayAttempts[0].InfrastructureReason == "" {
					t.Fatalf("eviction lost its infrastructure classification: %#v", completion)
				}
				return
			}
			if completion.Infrastructure || completion.FailedStep != "test" {
				t.Fatalf("declared budget classified as infrastructure: %#v", completion)
			}
			for _, attempt := range append(completion.ReplayAttempts, completion.Steps...) {
				if attempt.InfrastructureReason != "" {
					t.Fatal("retry evidence still marks the attempt evicted by infrastructure")
				}
			}
			want := "Step " + completion.Steps[0].Stage + "/test exceeded its declared ephemeral-storage budget: the kubelet evicted its Pod at the total container limit 1152Mi"
			if !strings.HasPrefix(completion.Reason, want) {
				t.Fatalf("reason %q does not name the declared total", completion.Reason)
			}
		})
	}
}
