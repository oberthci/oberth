package model

import (
	"testing"
	"time"
)

func TestResourceContainerReportsSeparateScopesAndDeduplicatePodOverlap(t *testing.T) {
	at := time.Now().UTC()
	p := func(n int64) *int64 { return &n }
	rows := []StepResourceUsage{}
	for i := 0; i < 20; i++ {
		rows = append(rows, StepResourceUsage{RunID: "run", PodUID: string(rune('a' + i)), Burn: "ci", Step: "test", ContainerName: "main", PodStartedAt: at, PodFinishedAt: at.Add(time.Second), MemoryPeakBytes: p(int64(i + 1)), EphemeralStoragePeakBytes: p(10), PodPVCUsedPeakBytes: p(100), Samples: 1})
	}
	wait := rows[0]
	wait.ContainerName = "wait"
	wait.MemoryPeakBytes = p(1000)
	rows = append(rows, wait)
	init := rows[0]
	init.ContainerName = "init"
	init.ContainerKind = "init"
	init.MemoryPeakBytes = nil
	init.Samples = 0
	rows = append(rows, init)
	vm := rows[0]
	vm.PodKind = "vm-launcher"
	vm.ContainerName = "compute"
	vm.PodUID = "vm"
	vm.MemoryPeakBytes = p(2000)
	rows = append(rows, vm)
	legacy := BuildResourceReport(rows)
	if len(legacy) != 1 || legacy[0].Metrics["memory_bytes"].Used.Count != 20 || *legacy[0].Metrics["memory_bytes"].Used.P95 != 19 || *legacy[0].ByWorkflowSize[0].MemoryPeakBytes.Max != 20 {
		t.Fatal("helper/VM polluted legacy main report", legacy)
	}
	containers := BuildContainerResourceReport(rows)
	if len(containers) != 4 {
		t.Fatal("container groups missing", containers)
	}
	for _, c := range containers {
		if c.ContainerName == "init" && (c.Unsampled != 1 || c.Metrics["memory_bytes"].Used.Max != nil) {
			t.Fatal("unknown became zero")
		}
	}
	pods, overlap := BuildPodResourceReport(rows)
	if len(pods) != 2 || overlap.Pods != 21 || overlap.TimedPods != 21 || *overlap.PerRunPeakPods.Max != 21 {
		t.Fatal("container rows inflated pod counts", pods, overlap)
	}
	for _, r := range pods {
		if r.PodKind == "workflow" && (r.PVCUsedPeakBytes.Count != 20 || *r.PVCUsedPeakBytes.Max != 100) {
			t.Fatal("shared pod PVC added", r)
		}
	}
	// Half-open intervals: a pod ending exactly at the next start does not overlap.
	a, b := rows[0], rows[1]
	a.PodFinishedAt = at.Add(time.Second)
	b.PodStartedAt = a.PodFinishedAt
	b.PodFinishedAt = at.Add(2 * time.Second)
	_, overlap = BuildPodResourceReport([]StepResourceUsage{a, b})
	if *overlap.PerRunPeakPods.Max != 1 {
		t.Fatal("touching intervals counted together", overlap)
	}
	b.PodFinishedAt = time.Time{}
	b.LastObservedAt = at.Add(2 * time.Second)
	b.ContainerStartedAt = b.PodStartedAt
	_, overlap = BuildPodResourceReport([]StepResourceUsage{a, b})
	if overlap.OpenPods != 1 || *overlap.PerRunPeakPods.Max != 1 {
		t.Fatal("open observation semantics", overlap)
	}
	b.PodStartedAt = time.Time{}
	_, overlap = BuildPodResourceReport([]StepResourceUsage{a, b})
	if overlap.Pods != 2 || overlap.TimedPods != 1 {
		t.Fatal("missing start invented", overlap)
	}
}
