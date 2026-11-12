package argoworkflow

import corev1 "k8s.io/api/core/v1"

// PodCPURequests follows Kubernetes scheduling requests: application containers
// plus restartable init sidecars, the largest init phase, then Pod overhead.
func PodCPURequests(pod *corev1.Pod) int64 { return podResourceRequests(pod, corev1.ResourceCPU) }

// PodMemoryRequests is the same scheduling envelope in bytes.
func PodMemoryRequests(pod *corev1.Pod) int64 { return podResourceRequests(pod, corev1.ResourceMemory) }

func podResourceRequests(pod *corev1.Pod, name corev1.ResourceName) int64 {
	value := func(resources corev1.ResourceList) int64 {
		q := resources[name]
		if name == corev1.ResourceCPU {
			return q.MilliValue()
		}
		return q.Value()
	}
	var app, sidecars, initial int64
	for _, c := range pod.Spec.Containers {
		app += value(c.Resources.Requests)
	}
	for _, c := range pod.Spec.InitContainers {
		cpu := value(c.Resources.Requests)
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars += cpu
			initial = max(initial, sidecars)
		} else {
			initial = max(initial, sidecars+cpu)
		}
	}
	cpu := max(app+sidecars, initial)
	if pod.Spec.Resources != nil {
		if _, ok := pod.Spec.Resources.Requests[name]; ok {
			cpu = value(pod.Spec.Resources.Requests)
		}
	}
	return cpu + value(pod.Spec.Overhead)
}
