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
			controller.collectRestartStepDiagnostics(context.Background(), wf, &completion, time.Time{})
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
			controller.collectRestartStepDiagnostics(context.Background(), wf, &completion, time.Time{})
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
			controller.collectRestartStepDiagnostics(context.Background(), wf, &completion, time.Time{})
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
	controller.collectRestartStepDiagnostics(t.Context(), wf, &completion, time.Time{})
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

// Only a named authored container's own limit proves a step resource failure.
// Pod totals include native operator-controlled helpers and stay infrastructure.
func TestDeclaredStorageEvictionIsStepFailure(t *testing.T) {
	const total = "Pod ephemeral local storage usage exceeds the total limit of containers 1152Mi. "
	mainOver := `Container main exceeded its local ephemeral storage limit "640Mi". `
	limit := corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceEphemeralStorage: resource.MustParse("640Mi")}}
	declared := wfv1.Template{Name: "test-template", Container: &corev1.Container{Resources: limit}}
	undeclared := wfv1.Template{Name: "test-template", Container: &corev1.Container{}}
	// Argo replaces container-set lists, so this default limit never runs.
	setDefaults := &wfv1.Template{ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main", Resources: limit}}}}}
	set := wfv1.Template{Name: "test-template", ContainerSet: &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main"}}}}}
	// Argo adds a default-only sidecar by name.
	sidecarDefaults := &wfv1.Template{Sidecars: []wfv1.UserContainer{{Container: corev1.Container{Name: "proxy", Resources: limit}}}}
	cases := []struct {
		name     string
		policy   string
		defaults *wfv1.Template
		template wfv1.Template
		nodeMsg  string
		podMsg   string
		reason   string // the enforced limit the run reason names; "" means infrastructure
	}{
		{name: "v4/node", policy: helperStoragePolicy, template: undeclared, nodeMsg: total},
		{name: "v4/node-prefixed", policy: helperStoragePolicy, template: declared, nodeMsg: "Evicted: " + total},
		{name: "v4/pod-only", policy: helperStoragePolicy, template: declared, nodeMsg: "Evicted", podMsg: total},
		// The kubelet prints Quantity.String(), which keeps exponent form.
		{name: "v4/exponent", policy: helperStoragePolicy, template: declared, nodeMsg: "Pod ephemeral local storage usage exceeds the total limit of containers 1e9. "},
		// Container-set default lists do not contribute the step limit.
		{name: "v4/containerset-defaults", policy: helperStoragePolicy, defaults: setDefaults, template: set, nodeMsg: total},

		// A step container's own limit.
		{name: "v4/container", policy: helperStoragePolicy, template: declared, nodeMsg: mainOver, reason: "container main's own limit 640Mi"},
		{name: "v4/container-pod-only", policy: helperStoragePolicy, template: declared, nodeMsg: "Evicted", podMsg: mainOver, reason: "container main's own limit 640Mi"},
		{name: "v4/default-sidecar", policy: helperStoragePolicy, defaults: sidecarDefaults, template: undeclared, nodeMsg: `Container proxy exceeded its local ephemeral storage limit "640Mi". `, reason: "container proxy's own limit 640Mi"},
		// A server helper's limit is not the step's budget.
		{name: "v4/helper-container", policy: helperStoragePolicy, template: declared, nodeMsg: `Container wait exceeded its local ephemeral storage limit "512Mi". `},
		{name: "v4/container-undeclared", policy: helperStoragePolicy, template: undeclared, nodeMsg: mainOver},
		{name: "v4/container-unresolved-template", policy: helperStoragePolicy, template: wfv1.Template{Name: "other", Container: declared.Container}, nodeMsg: mainOver},
		{name: "v4/containerset-defaults-container", policy: helperStoragePolicy, defaults: setDefaults, template: set, nodeMsg: mainOver},
		// Node pressure is never the step's budget.
		{name: "v4/node-pressure", policy: helperStoragePolicy, template: declared, nodeMsg: "The node was low on resource: ephemeral-storage. Threshold quantity: 1Gi, available: 123Ki."},
		{name: "v4/pod-node-pressure", policy: helperStoragePolicy, template: declared, nodeMsg: "Evicted", podMsg: "The node was low on resource: ephemeral-storage. "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf, node, pod, _ := diagnosticFixture()
			wf.Annotations[helperStorageAnnotation] = tc.policy
			wf.Spec.TemplateDefaults = tc.defaults
			wf.Spec.Templates = []wfv1.Template{tc.template}
			node.TemplateName = "test-template"
			node.Message = tc.nodeMsg
			wf.Status.Nodes[node.ID] = node
			pod.Name = fixturePodName(wf, node)
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
			controller.collectRestartStepDiagnostics(context.Background(), wf, &completion, time.Time{})
			if completion.Succeeded {
				t.Fatal("evicted step succeeded")
			}
			if tc.reason == "" {
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
			want := "Step " + completion.Steps[0].Stage + "/test exceeded its declared ephemeral-storage budget: the kubelet evicted its Pod at " + tc.reason
			if !strings.HasPrefix(completion.Reason, want) {
				t.Fatalf("reason %q does not name the declared limit %q", completion.Reason, tc.reason)
			}
		})
	}
}

// Every form resource.Quantity.String() prints parses back to the same total.
