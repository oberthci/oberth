package argojob

import (
	"fmt"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// CPU/memory remain provisional. The pinned Argo v4.0.8 executor occupies
// 160825009 bytes in the shared disk emptyDir before workload scratch/logs.
// Reserve helper-owned storage in the Pod sum; init limits alone do not raise
// the steady-state Pod allowance. Headroom is provisional, not a measured peak.
func executorControlResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"), corev1.ResourceEphemeralStorage: resource.MustParse("512Mi")},
	}
}

const helperStorageAnnotation = "oberth.ci/helper-storage"
const helperStoragePolicy = "argo-256Mi-512Mi-v1"

// Reconstruct the immutable resources of already-submitted Workflows. This
// switch is private to recovery; authored annotations cannot opt new runs out.
func recoverHelperStorage(wf *wfv1.Workflow, request *Request) error {
	policy := wf.Annotations[helperStorageAnnotation]
	if policy != "" && policy != helperStoragePolicy {
		return fmt.Errorf("argojob: existing workflow has an unknown helper-storage policy")
	}
	request.legacyHelperStorage = policy == ""
	return nil
}

func executorResourcesWithStorage(enabled bool) corev1.ResourceRequirements {
	resources := executorControlResources()
	if !enabled {
		delete(resources.Requests, corev1.ResourceEphemeralStorage)
		delete(resources.Limits, corev1.ResourceEphemeralStorage)
	}
	return resources
}
