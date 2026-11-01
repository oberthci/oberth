package model

import (
	"strings"
	"testing"
	"time"
)

func resourceModelRow() StepResourceUsage {
	at := time.Now().UTC()
	return StepResourceUsage{Version: 2, Source: "kubelet-cadvisor", RunID: strings.Repeat("a", 32), WorkflowName: "wf", WorkflowUID: "11111111-1111-4111-8111-111111111111", Namespace: "argo", NodeID: "wf-1", PodName: "wf-1", PodUID: "22222222-2222-4222-8222-222222222222", NodeName: "node-a", ContainerID: "containerd://" + strings.Repeat("b", 64), ContainerName: "main", ContainerKind: "app", PodKind: "workflow", Burn: "ci", Step: "test", FirstObservedAt: at, LastObservedAt: at, SourceAt: at, ContainerSourceAt: at, IOPressureAt: at, Samples: 1, SampledLowerBound: true, ContainerStartedAt: at.Add(-time.Minute)}
}

func TestResourceKindsTimingAndGaugeMergeGuards(t *testing.T) {
	v := resourceModelRow()
	if err := v.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"container", "pod", "start"} {
		next := v
		switch kind {
		case "container":
			next.ContainerKind = "init"
		case "pod":
			next.PodKind = "vm-launcher"
		case "start":
			next.ContainerStartedAt = next.ContainerStartedAt.Add(time.Second)
		}
		if _, err := MergeResourceUsage(v, next); err == nil {
			t.Fatal("binding changed", kind)
		}
	}
	for _, kind := range []string{"container", "pod"} {
		bad := v
		if kind == "container" {
			bad.ContainerKind = "invented"
		} else {
			bad.PodKind = "invented"
		}
		if bad.Validate() == nil {
			t.Fatal("unknown kind accepted", kind)
		}
	}
	oldValue, newValue := int64(100), int64(20)
	v.RootfsPeakBytes = &oldValue
	v.PodMemoryPeakBytes = &oldValue
	next := v
	next.RootfsPeakBytes = &newValue
	next.PodMemoryPeakBytes = &newValue
	next.LastObservedAt = v.LastObservedAt.Add(time.Second)
	next.IOPressureAt = next.LastObservedAt
	merged, err := MergeResourceUsage(v, next)
	if err != nil || merged.CounterReset || *merged.RootfsPeakBytes != 100 || *merged.PodMemoryPeakBytes != 100 {
		t.Fatal("falling gauges became counter resets", merged, err)
	}
}

func TestResourceOverlapUsesLastRunningObservation(t *testing.T) {
	at := time.Now().UTC()
	a := StepResourceUsage{RunID: "run", PodUID: "a", PodStartedAt: at, ContainerStartedAt: at, LastObservedAt: at.Add(2 * time.Second)}
	b := StepResourceUsage{RunID: "run", PodUID: "b", PodStartedAt: at.Add(time.Second), PodFinishedAt: at.Add(3 * time.Second)}
	_, r := BuildPodResourceReport([]StepResourceUsage{a, b})
	if r.TimedPods != 2 || r.OpenPods != 1 || r.PerRunPeakPods.Max == nil || *r.PerRunPeakPods.Max != 2 {
		t.Fatal("running observation end lost", r)
	}
}
