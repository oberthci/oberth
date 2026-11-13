package argojob

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

// Fixtures choose opaque Pod names; no copy of Argo's naming convention is used.
func fixturePodName(_ *wfv1.Workflow, node wfv1.NodeStatus) string { return node.ID }

func fixtureStepPods(workflow *wfv1.Workflow) map[string]*corev1.Pod {
	if workflow.UID == "" {
		workflow.UID = "fixture-workflow-uid"
	}
	pods := make(map[string]*corev1.Pod)
	for _, node := range workflow.Status.Nodes {
		if node.Type != wfv1.NodeTypePod {
			continue
		}
		name := fixturePodName(workflow, node)
		pods[name] = &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: workflow.Namespace, UID: types.UID("fixture-" + name),
			Labels:          map[string]string{"workflows.argoproj.io/workflow": workflow.Name},
			Annotations:     map[string]string{"workflows.argoproj.io/node-id": node.ID, "workflows.argoproj.io/node-name": node.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: workflow.Name, UID: workflow.UID, Controller: ptr.To(true)}}}}
	}
	return pods
}

func fixtureKube(workflow *wfv1.Workflow) *fake.Clientset {
	var objects []runtime.Object
	for _, pod := range fixtureStepPods(workflow) {
		objects = append(objects, pod)
	}
	return fake.NewClientset(objects...)
}

func fixtureStepAttempts(workflow *wfv1.Workflow) []StepResult {
	steps := stepAttempts(workflow)
	if workflow != nil {
		bindStepPodNames(workflow, steps, fixtureStepPods(workflow))
	}
	return steps
}

func fixtureStepResults(workflow *wfv1.Workflow) []StepResult {
	return finalStepResults(fixtureStepAttempts(workflow))
}

func TestStepPodDiscoveryUsesAnnotationsAndOwnerWithOpaqueNames(t *testing.T) {
	for _, format := range []string{"", "v1", "v2"} {
		t.Run(format, func(t *testing.T) {
			workflow, node, pod, _ := diagnosticFixture()
			workflow.Annotations["workflows.argoproj.io/pod-name-format"] = format
			pod.Name = "opaque-observed-step"
			controller := &Controller{hub: containerLogCache{workflow: workflow, pods: []*corev1.Pod{pod}}}
			steps := controller.observedStepAttempts(t.Context(), workflow)
			if len(steps) != 1 || steps[0].PodName != pod.Name || steps[0].NodeID != node.ID {
				t.Fatalf("Pod was not discovered from native identity: %+v", steps)
			}
			if StepResults(workflow)[0].PodName != "" {
				t.Fatal("status-only projection guessed a Pod name")
			}
		})
	}
}

func TestStepPodDiscoveryRejectsForeignAmbiguousAndMissingPods(t *testing.T) {
	for _, mode := range []string{"absent", "owner", "namespace", "node-id", "node-name", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			workflow, _, pod, _ := diagnosticFixture()
			pod.Name = "opaque-observed-step"
			var objects []runtime.Object
			switch mode {
			case "owner":
				pod.OwnerReferences[0].UID = "another-workflow"
			case "namespace":
				pod.Namespace = "another-namespace"
			case "node-id":
				pod.Annotations["workflows.argoproj.io/node-id"] = "another-node"
			case "node-name":
				pod.Annotations["workflows.argoproj.io/node-name"] = "another-attempt"
			case "duplicate":
				other := pod.DeepCopy()
				other.Name, other.UID = "second-observed-step", "second-pod-uid"
				objects = append(objects, other)
			}
			if mode != "absent" {
				objects = append(objects, pod)
			}
			var pods []*corev1.Pod
			for _, object := range objects {
				pods = append(pods, object.(*corev1.Pod))
			}
			controller := &Controller{hub: containerLogCache{workflow: workflow, pods: pods}}
			steps := controller.observedStepAttempts(t.Context(), workflow)
			if len(steps) != 1 || steps[0].PodName != "" {
				t.Fatalf("unsafe or guessed binding: %+v", steps)
			}
		})
	}
}
