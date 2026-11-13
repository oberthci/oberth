package argojob

import (
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func nativeContainerFixture() (*wfv1.Workflow, *corev1.Pod) {
	workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "native-run", Namespace: "pipelines", UID: "workflow-uid"}}
	workflow.Status.Nodes = wfv1.Nodes{
		"pod-node":       {ID: "pod-node", Name: "native-run.build", DisplayName: "build", Type: wfv1.NodeTypePod},
		"container-node": {ID: "container-node", Name: "native-run.build.compile", DisplayName: "compile", BoundaryID: "pod-node", Type: wfv1.NodeTypeContainer},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "opaque-pod-name", Namespace: workflow.Namespace, UID: "pod-uid",
		Annotations:     map[string]string{"workflows.argoproj.io/node-id": "pod-node", "workflows.argoproj.io/node-name": "native-run.build"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: workflow.Name, UID: workflow.UID, Controller: ptr.To(true)}}},
		Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: "init"}}, Containers: []corev1.Container{{Name: "compile"}, {Name: "wait"}}}}
	return workflow, pod
}

func TestStepContainersUsesNativeIdentityAndContainerNodes(t *testing.T) {
	workflow, pod := nativeContainerFixture()
	beforeWorkflow, beforePod := workflow.DeepCopy(), pod.DeepCopy()
	got := StepContainers(workflow, []*corev1.Pod{pod})
	want := []StepContainer{
		{NodeID: "container-node", Namespace: pod.Namespace, PodName: pod.Name, ContainerName: "compile", PodUID: pod.UID},
		{NodeID: "pod-node", Namespace: pod.Namespace, PodName: pod.Name, ContainerName: "init", PodUID: pod.UID},
		{NodeID: "pod-node", Namespace: pod.Namespace, PodName: pod.Name, ContainerName: "wait", PodUID: pod.UID},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mapping = %+v; want %+v", got, want)
	}
	if !reflect.DeepEqual(beforeWorkflow, workflow) || !reflect.DeepEqual(beforePod, pod) {
		t.Fatal("mapping changed a shared cache object")
	}
}

func TestStepContainersRejectsForeignAndAmbiguousPods(t *testing.T) {
	for _, mode := range []string{"namespace", "uid", "owner", "controllers", "node-id", "node-name", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			workflow, pod := nativeContainerFixture()
			pods := []*corev1.Pod{nil, pod}
			switch mode {
			case "namespace":
				pod.Namespace = "other"
			case "uid":
				pod.UID = ""
			case "owner":
				pod.OwnerReferences[0].UID = "other"
			case "controllers":
				pod.OwnerReferences = append(pod.OwnerReferences, pod.OwnerReferences[0])
			case "node-id":
				pod.Annotations["workflows.argoproj.io/node-id"] = "other"
			case "node-name":
				pod.Annotations["workflows.argoproj.io/node-name"] = "other"
			case "duplicate":
				other := pod.DeepCopy()
				other.Name, other.UID = "another-pod", "another-uid"
				pods = append(pods, other)
			}
			if got := StepContainers(workflow, pods); len(got) != 0 {
				t.Fatalf("unsafe mapping: %+v", got)
			}
		})
	}
	workflow, pod := nativeContainerFixture()
	foreign := pod.DeepCopy()
	foreign.OwnerReferences[0].UID = "other"
	if got := StepContainers(workflow, []*corev1.Pod{foreign, pod, pod}); len(got) != 3 {
		t.Fatalf("foreign or repeated cache input hid the owned Pod: %+v", got)
	}
}
