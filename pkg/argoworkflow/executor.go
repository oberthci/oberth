package argoworkflow

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ExecutorImage pins the official executor index used for executor token delivery.
const ExecutorImage = "quay.io/argoproj/argoexec:v4.0.8@sha256:86a965d7eea176959351156e24f3a2fa8d8e342e477ef425009af95a12e3fe87"

// ExecutorResources defines the fixed helper budgets. CPU/memory remain provisional.
// The pinned Argo v4.0.8 executor occupies
// 160825009 bytes in the shared disk emptyDir before workload scratch/logs.
// Reserve helper-owned storage in the Pod sum; init limits alone do not raise
// the steady-state Pod allowance. Headroom is provisional, not a measured peak.
// Server-owned helpers may omit the storage limit for steps without one.
func ExecutorResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("512Mi")},
	}
}
