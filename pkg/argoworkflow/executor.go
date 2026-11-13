package argoworkflow

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ExecutorImage pins the official executor index used for executor token delivery.
const ExecutorImage = "quay.io/argoproj/argoexec:v4.0.8@sha256:86a965d7eea176959351156e24f3a2fa8d8e342e477ef425009af95a12e3fe87"

// ExecutorResources supplies native executor CPU/memory requests. Limits are
// operator-owned: a helper storage limit would cap otherwise unbounded Pods.
func ExecutorResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("50m"),
		corev1.ResourceMemory: resource.MustParse("64Mi"),
	}}
}
