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
	values := ObserveContainers(b, pod, snapshot, at)
	if len(values) != 4 {
		t.Fatalf("missing containers: %+v", values)
	}
	for _, v := range values {
		if v.Version != 2 || v.MemoryPeakBytes == nil || !v.ContainerStartedAt.Equal(started.Time) || !v.PodStartedAt.Equal(started.Time) || v.Validate() != nil {
			t.Fatalf("bad container observation: %+v", v)
		}
		if v.ContainerName == "init" && v.ContainerKind != "init" {
			t.Fatal("init scope lost")
		}
	}
	// ContainerSets need not have a container called main.
	pod.Spec.Containers = pod.Spec.Containers[1:]
	pod.Status.ContainerStatuses = pod.Status.ContainerStatuses[1:]
	if values = ObserveContainers(b, pod, snapshot, at); len(values) != 3 {
		t.Fatalf("mainless set lost: %+v", values)
	}
	// A scrape failure still records independently observed timing, never usage.
	values = ObserveContainers(b, pod, Snapshot{}, at)
	if len(values) != 3 || values[0].Samples != 0 || values[0].MemoryPeakBytes != nil {
		t.Fatal("missing became measured", values)
	}
	pod.OwnerReferences[0].UID = "foreign"
	if len(ObserveContainers(b, pod, snapshot, at)) != 0 {
		t.Fatal("foreign helpers accepted")
	}
}

func TestResourceStorageSummaryRequiresIncarnationAndCompletePVCStats(t *testing.T) {
	b, pod, snapshot, at := sampleFixture(t)
	start := pod.Status.ContainerStatuses[0].State.Running.StartedAt.Time
	pod.Spec.Volumes = []corev1.Volume{{Name: "source", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "source-pvc"}}}, {Name: "cache", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "cache-pvc"}}}}
	raw := fmt.Sprintf(`{"pods":[{"podRef":{"name":%q,"namespace":%q,"uid":%q},"cpu":{"time":%q,"usageNanoCores":1234},"memory":{"time":%q,"usageBytes":5678},"containers":[{"name":"main","startTime":%q,"rootfs":{"time":%q,"usedBytes":12},"logs":{"time":%q,"usedBytes":0}}],"volume":[{"name":"source","pvcRef":{"name":"source-pvc","namespace":%q},"time":%q,"usedBytes":100},{"name":"cache","pvcRef":{"name":"cache-pvc","namespace":%q},"time":%q,"usedBytes":200}]}]}`, pod.Name, pod.Namespace, pod.UID, at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), start.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), pod.Namespace, at.Format(time.RFC3339Nano), pod.Namespace, at.Format(time.RFC3339Nano))
	if err := json.Unmarshal([]byte(raw), &snapshot.Summary); err != nil {
		t.Fatal(err)
	}
	snapshot.SummaryOK = true
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
