package resourcesampler

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// Synthetic observation CPU/allocation cost only: excludes HTTP, JSON parsing
// and database writes. The shared index is rebuilt on each benchmark iteration.
func BenchmarkResourceContainerSampling(b *testing.B) {
	now := time.Now().UTC().Truncate(time.Second)
	binding := Binding{RunID: strings.Repeat("a", 32), WorkflowName: "wf", WorkflowUID: "11111111-1111-4111-8111-111111111111", Namespace: "argo", NodeID: "wf-123", NodeName: "wf.test", PodName: "wf-123", Stage: "ci", Step: "test"}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: binding.PodName, Namespace: binding.Namespace, UID: "22222222-2222-4222-8222-222222222222", Annotations: map[string]string{"workflows.argoproj.io/node-id": binding.NodeID, "workflows.argoproj.io/node-name": binding.NodeName}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: binding.WorkflowName, UID: "11111111-1111-4111-8111-111111111111", Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{NodeName: "node-a"}}
	snapshot := Snapshot{MetricsOK: true}
	for i := 0; i < 8; i++ {
		name, cid := fmt.Sprintf("c%d", i), fmt.Sprintf("%064x", i+1)
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: name})
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{Name: name, ContainerID: "containerd://" + cid, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(now.Add(-time.Minute))}}})
		for j := 0; j < 200; j++ {
			podName, uid := fmt.Sprintf("foreign-%d", j), fmt.Sprintf("foreign-%d", j)
			if j == 0 {
				podName, uid = pod.Name, string(pod.UID)
			}
			for _, family := range []string{"container_memory_usage_bytes", "container_memory_max_usage_bytes", "container_cpu_usage_seconds_total", "container_fs_reads_bytes_total", "container_fs_writes_bytes_total", "container_fs_reads_total"} {
				snapshot.Metrics = append(snapshot.Metrics, Metric{Name: family, At: now, Value: 123, Labels: map[string]string{"namespace": pod.Namespace, "pod": podName, "container": name, "cpu": "total", "device": "disk", "id": "/kubepods/pod" + uid + "/" + cid}})
			}
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		indexed := IndexSnapshot(snapshot)
		for range 200 {
			if len(ObserveContainers(binding, pod, indexed, now)) != 8 {
				b.Fatal("observation lost")
			}
		}
	}
}
