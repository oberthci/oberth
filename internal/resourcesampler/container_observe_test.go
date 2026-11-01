package resourcesampler

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
)

func TestResourceContainersIncludeHelpersAndMainlessContainerSet(t *testing.T) {
	b, pod, snapshot, at := sampleFixture(t)
	started := pod.Status.ContainerStatuses[0].State.Running.StartedAt
	pod.Status.StartTime = &started
	for i, name := range []string{"wait", "build", "init"} {
		status := pod.Status.ContainerStatuses[0]
		status.Name, status.ContainerID = name, "containerd://"+strings.Repeat(fmt.Sprintf("%x", i+12), 64)
		container := corev1.Container{Name: name}
		if name == "init" {
			pod.Spec.InitContainers = append(pod.Spec.InitContainers, container)
			pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, status)
		} else {
			pod.Spec.Containers = append(pod.Spec.Containers, container)
			pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, status)
		}
		metric := snapshot.Metrics[0]
		metric.Name, metric.Value = "container_memory_usage_bytes", float64(100+i)
		metric.Labels = map[string]string{"namespace": pod.Namespace, "pod": pod.Name, "container": name, "id": "/kubepods/pod" + string(pod.UID) + "/" + strings.TrimPrefix(status.ContainerID, "containerd://")}
		snapshot.Metrics = append(snapshot.Metrics, metric)
	}
	ephemeral := pod.Status.ContainerStatuses[0]
	ephemeral.Name, ephemeral.ContainerID = "debug", "containerd://"+strings.Repeat("f", 64)
	pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug"}}}
	pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{ephemeral}
	metric := Metric{Name: "container_memory_usage_bytes", At: at, Value: 104, Labels: map[string]string{"namespace": pod.Namespace, "pod": pod.Name, "container": "debug", "id": "/kubepods/pod" + string(pod.UID) + "/" + strings.Repeat("f", 64)}}
	snapshot.Metrics = append(snapshot.Metrics, metric)
	values := ObserveContainers(b, pod, snapshot, at)
	if len(values) != 5 {
		t.Fatalf("missing containers: %+v", values)
	}
	for _, v := range values {
		if v.Version != 2 || v.MemoryPeakBytes == nil || !v.ContainerStartedAt.Equal(started.Time) || !v.PodStartedAt.Equal(started.Time) || v.Validate() != nil {
			t.Fatalf("bad container observation: %+v", v)
		}
		if v.ContainerName == "debug" && v.ContainerKind != "ephemeral" {
			t.Fatal("ephemeral scope lost")
		}
		if v.ContainerName == "init" && v.ContainerKind != "init" {
			t.Fatal("init scope lost")
		}
	}
	// ContainerSets need not have a container called main.
	pod.Spec.Containers = pod.Spec.Containers[1:]
	pod.Status.ContainerStatuses = pod.Status.ContainerStatuses[1:]
	if values = ObserveContainers(b, pod, snapshot, at); len(values) != 4 {
		t.Fatalf("mainless set lost: %+v", values)
	}
	// A scrape failure still records independently observed timing, never usage.
	values = ObserveContainers(b, pod, Snapshot{}, at)
	if len(values) != 4 || values[0].Samples != 0 || values[0].MemoryPeakBytes != nil {
		t.Fatal("missing became measured", values)
	}
	pod.OwnerReferences[0].UID = "foreign"
	if len(ObserveContainers(b, pod, snapshot, at)) != 0 {
		t.Fatal("foreign helpers accepted")
	}
}

func storageFixture(t *testing.T) (Binding, *corev1.Pod, Snapshot, time.Time) {
	t.Helper()
	b, pod, snapshot, at := sampleFixture(t)
	start := pod.Status.ContainerStatuses[0].State.Running.StartedAt.Time
	// Pinned upstream CRI stats uses container.CreatedAt, two seconds before
	// Running.StartedAt here. The previous incarnation ended before creation.
	created := start.Add(-2 * time.Second)
	pod.Status.ContainerStatuses[0].RestartCount = 1
	pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{FinishedAt: metav1.NewTime(start.Add(-3 * time.Second))}
	pod.Spec.Volumes = []corev1.Volume{{Name: "source", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "source-pvc"}}}, {Name: "cache", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "cache-pvc"}}}}
	raw := fmt.Sprintf(`{"pods":[{"podRef":{"name":%q,"namespace":%q,"uid":%q},"cpu":{"time":%q,"usageNanoCores":1234},"memory":{"time":%q,"usageBytes":5678},"containers":[{"name":"main","startTime":%q,"rootfs":{"time":%q,"usedBytes":12},"logs":{"time":%q,"usedBytes":0}}],"volume":[{"name":"source","pvcRef":{"name":"source-pvc","namespace":%q},"time":%q,"usedBytes":100},{"name":"cache","pvcRef":{"name":"cache-pvc","namespace":%q},"time":%q,"usedBytes":200}]}]}`, pod.Name, pod.Namespace, pod.UID, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), created.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), pod.Namespace, at.Format(time.RFC3339Nano), pod.Namespace, at.Format(time.RFC3339Nano))
	if err := json.Unmarshal([]byte(raw), &snapshot.Summary); err != nil {
		t.Fatal(err)
	}
	snapshot.SummaryOK = true
	return b, pod, snapshot, at
}

func TestResourceStorageSummaryRequiresIncarnationAndCompletePVCStats(t *testing.T) {
	b, pod, snapshot, at := storageFixture(t)
	start := pod.Status.ContainerStatuses[0].State.Running.StartedAt.Time
	v, ok := Observe(b, pod, snapshot, at)
	if !ok || v.RootfsPeakBytes == nil || *v.RootfsPeakBytes != 12 || v.LogsPeakBytes == nil || *v.LogsPeakBytes != 0 || v.PodPVCUsedPeakBytes == nil || *v.PodPVCUsedPeakBytes != 300 || v.PodCPUPeakNanoCores == nil || *v.PodCPUPeakNanoCores != 1234 || v.PodMemoryPeakBytes == nil || *v.PodMemoryPeakBytes != 5678 {
		t.Fatalf("storage: %+v", v)
	}
	// An older incarnation at the same name cannot supply storage, nor can
	// one fresh volume hide an unknown/stale sibling or a foreign claim.
	for _, bad := range []string{"incarnation", "stale-volume", "skewed-volume", "foreign-volume", "foreign-pod"} {
		t.Run(bad, func(t *testing.T) {
			s := snapshot
			s.Summary.Pods = append([]PodStats(nil), snapshot.Summary.Pods...)
			s.Summary.Pods[0].Containers = append([]ContainerStats(nil), snapshot.Summary.Pods[0].Containers...)
			s.Summary.Pods[0].Volumes = append([]VolumeStats(nil), snapshot.Summary.Pods[0].Volumes...)
			switch bad {
			case "incarnation":
				s.Summary.Pods[0].Containers[0].StartTime = start.Add(-time.Minute)
			case "stale-volume":
				s.Summary.Pods[0].Volumes[1].Time = at.Add(-MaxSampleAge - time.Second)
			case "skewed-volume":
				s.Summary.Pods[0].Volumes[1].Time = at.Add(-time.Second)
			case "foreign-volume":
				s.Summary.Pods[0].Volumes[1].PVCRef = nil
			case "foreign-pod":
				s.Summary.Pods[0].PodRef.UID = "foreign"
			}
			v, _ := Observe(b, pod, s, at)
			if bad == "incarnation" {
				if v.RootfsPeakBytes != nil || v.LogsPeakBytes != nil {
					t.Fatal("reused container supplied storage")
				}
			} else if v.PodPVCUsedPeakBytes != nil {
				t.Fatal("partial/foreign volume usage accepted")
			}
		})
	}
}

func TestResourceTimingFinishesAndMissingSamplesMerge(t *testing.T) {
	b, pod, snapshot, at := sampleFixture(t)
	started := pod.Status.ContainerStatuses[0].State.Running.StartedAt
	pod.Status.StartTime = &started
	first := ObserveContainers(b, pod, snapshot, at)[0]
	next := ObserveContainers(b, pod, Snapshot{}, at.Add(time.Second))[0]
	merged, err := model.MergeResourceUsage(first, next)
	if err != nil || !merged.LastObservedAt.Equal(at.Add(time.Second)) || merged.MemoryPeakBytes == nil || merged.Samples != 1 {
		t.Fatal("timing outage erased last evidence", merged, err)
	}
	pod.Status.Phase = corev1.PodSucceeded
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: started, FinishedAt: metav1.NewTime(at.Add(2 * time.Second)), ExitCode: 0, Reason: "Completed"}}
	last := ObserveContainers(b, pod, Snapshot{}, at.Add(3*time.Second))[0]
	merged, err = model.MergeResourceUsage(merged, last)
	if err != nil || merged.PodFinishedAt.IsZero() || merged.ContainerFinishedAt.IsZero() || !merged.Terminal || *merged.MemoryPeakBytes != 123456 {
		t.Fatal("finish lost", merged, err)
	}
	pod.Status.ContainerStatuses[0].RestartCount++
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("f", 64)
	if len(ObserveContainers(b, pod, snapshot, at.Add(3*time.Second))) != 1 {
		t.Fatal("new incarnation timing not recorded")
	}
	// Old cAdvisor data may never cross into the new container incarnation.
	if v := ObserveContainers(b, pod, snapshot, at.Add(3*time.Second))[0]; v.MemoryPeakBytes != nil {
		t.Fatal("old incarnation usage accepted")
	}
}

func TestResourceContainerObservationBound(t *testing.T) {
	b, pod, s, at := sampleFixture(t)
	for range 128 {
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, pod.Status.ContainerStatuses[0])
	}
	if len(ObserveContainers(b, pod, s, at)) != 0 {
		t.Fatal("oversized pod not bounded")
	}
}

func TestResourceSummaryCreationBoundsAndFreshness(t *testing.T) {
	for _, change := range []string{"first-incarnation", "missing-previous-finish", "old-incarnation", "after-start", "pod-name", "pod-namespace", "stale-pvc", "stale-rootfs", "stale-logs", "stale-pod-cpu", "duplicate-claim", "generic-ephemeral"} {
		t.Run(change, func(t *testing.T) {
			b, p, s, at := storageFixture(t)
			stats := &s.Summary.Pods[0]
			wantStorage := true
			switch change {
			case "first-incarnation":
				p.Status.ContainerStatuses[0].RestartCount = 0
				p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
			case "missing-previous-finish":
				p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{}
				wantStorage = false
			case "old-incarnation":
				stats.Containers[0].StartTime = p.Status.ContainerStatuses[0].LastTerminationState.Terminated.FinishedAt.Add(-time.Second)
				wantStorage = false
			case "after-start":
				stats.Containers[0].StartTime = p.Status.ContainerStatuses[0].State.Running.StartedAt.Add(time.Second)
				wantStorage = false
			case "pod-name":
				stats.PodRef.Name = "foreign"
				wantStorage = false
			case "pod-namespace":
				stats.PodRef.Namespace = "foreign"
				wantStorage = false
			case "stale-pvc":
				for i := range stats.Volumes {
					stats.Volumes[i].Time = at.Add(-MaxSampleAge - time.Second)
				}
			case "stale-rootfs":
				stats.Containers[0].Rootfs.Time = at.Add(-MaxSampleAge - time.Second)
				wantStorage = false
			case "stale-logs":
				stats.Containers[0].Logs.Time = at.Add(-MaxSampleAge - time.Second)
			case "stale-pod-cpu":
				stats.CPU.Time = at.Add(-MaxSampleAge - time.Second)
			case "duplicate-claim":
				v := p.Spec.Volumes[0]
				v.Name = "same-source"
				p.Spec.Volumes = append(p.Spec.Volumes, v)
				vs := stats.Volumes[0]
				vs.Name = v.Name
				stats.Volumes = append(stats.Volumes, vs)
			case "generic-ephemeral":
				p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "ephemeral", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{}}})
			}
			v, ok := Observe(b, p, s, at)
			if !ok || (v.RootfsPeakBytes != nil) != wantStorage {
				t.Fatal("creation binding or rootfs freshness", change, v)
			}
			if (change == "stale-logs" || change == "missing-previous-finish" || change == "old-incarnation" || change == "after-start" || change == "pod-name" || change == "pod-namespace") && v.LogsPeakBytes != nil {
				t.Fatal("unbound/stale logs accepted", v)
			}
			if (change == "stale-pvc" || change == "generic-ephemeral" || change == "pod-name" || change == "pod-namespace") && v.PodPVCUsedPeakBytes != nil {
				t.Fatal("unknown PVC total accepted", v)
			}
			if (change == "stale-pod-cpu" || change == "pod-name" || change == "pod-namespace") && v.PodCPUPeakNanoCores != nil {
				t.Fatal("unbound/stale pod CPU accepted", v)
			}
			if change == "duplicate-claim" && (v.PodPVCUsedPeakBytes == nil || *v.PodPVCUsedPeakBytes != 300) {
				t.Fatal("duplicate PVC counted", v)
			}
		})
	}
}

func TestResourceSkewedTimingDoesNotDiscardUsage(t *testing.T) {
	b, p, s, at := sampleFixture(t)
	future := metav1.NewTime(at.Add(time.Minute))
	p.CreationTimestamp = future
	p.Status.StartTime = &future
	p.Status.ContainerStatuses[0].State.Running.StartedAt = future
	v, ok := Observe(b, p, s, at)
	if !ok || v.MemoryPeakBytes == nil || !v.PodCreatedAt.IsZero() || !v.PodStartedAt.IsZero() || !v.ContainerStartedAt.IsZero() {
		t.Fatal("skewed timing discarded independent usage", v)
	}
}

func TestResourceDeclarationAndPodFinishGuards(t *testing.T) {
	b, p, s, at := sampleFixture(t)
	for _, duplicate := range []bool{false, true} {
		copy := p.DeepCopy()
		if duplicate {
			copy.Spec.Containers = append(copy.Spec.Containers, copy.Spec.Containers[0])
		} else {
			copy.Spec.Containers = nil
		}
		if _, ok := Observe(b, copy, s, at); ok {
			t.Fatal("undeclared/duplicate container accepted")
		}
	}
	started := p.Status.ContainerStatuses[0].State.Running.StartedAt
	p.Status.StartTime = &started
	p.Status.Phase = corev1.PodSucceeded
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{StartedAt: started, FinishedAt: metav1.NewTime(at)}}
	if PodFinishedAt(p).IsZero() {
		t.Fatal("known finish lost")
	}
	for _, kind := range []string{"app", "init", "ephemeral"} {
		copy := p.DeepCopy()
		switch kind {
		case "app":
			copy.Spec.Containers = append(copy.Spec.Containers, corev1.Container{Name: "missing"})
		case "init":
			copy.Spec.InitContainers = []corev1.Container{{Name: "missing"}}
		case "ephemeral":
			copy.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "missing"}}}
		}
		if !PodFinishedAt(copy).IsZero() {
			t.Fatal("partial statuses established pod finish", kind)
		}
	}
}
