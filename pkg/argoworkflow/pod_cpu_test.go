package argoworkflow

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func TestPodCPURequestsIncludesInitSidecarsOverheadAndPodBudget(t *testing.T) {
	container := func(cpu string) corev1.Container {
		return corev1.Container{Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}}
	}
	sidecar := container("500m")
	sidecar.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{container("2"), container("1")}, InitContainers: []corev1.Container{sidecar, container("4")}, Overhead: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}}}
	if n := PodCPURequests(pod); n != 4600 {
		t.Fatalf("effective request %dm, want 4600m", n)
	}
	pod.Spec.Resources = &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("5")}}
	if n := PodCPURequests(pod); n != 5100 {
		t.Fatalf("Pod-level budget %dm", n)
	}
}
