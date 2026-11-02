package model

import (
	"cmp"
	"slices"
	"strings"
	"time"
)

// ResourceContainerReport keeps helpers and launcher totals separate from the
// legacy workflow/main report. Quantiles describe per-incarnation peaks, never
// time-series p95 or simultaneous whole-run resource demand.
type ResourceContainerReport struct {
	Stage         string                          `json:"burn"`
	Step          string                          `json:"step"`
	ContainerName string                          `json:"container_name"`
	ContainerKind string                          `json:"container_kind"`
	PodKind       string                          `json:"pod_kind"`
	Observations  int                             `json:"observations"`
	Unsampled     int                             `json:"unsampled"`
	OOMKilled     int                             `json:"oom_killed"`
	Flagged       int                             `json:"flagged"`
	Metrics       map[string]ResourceMetricReport `json:"metrics"`
}

type ResourcePodReport struct {
	Stage                     string            `json:"burn"`
	Step                      string            `json:"step"`
	PodKind                   string            `json:"pod_kind"`
	Pods                      int               `json:"pods"`
	CPUPeakNanoCores          ResourceQuantiles `json:"cpu_peak_nanocores"`
	MemoryPeakBytes           ResourceQuantiles `json:"memory_peak_bytes"`
	EphemeralStoragePeakBytes ResourceQuantiles `json:"ephemeral_storage_peak_bytes"`
	PVCUsedPeakBytes          ResourceQuantiles `json:"pvc_used_peak_bytes"`
}

type ResourceOverlapReport struct {
	Semantics      string            `json:"semantics"`
	Pods           int               `json:"pods"`
	TimedPods      int               `json:"timed_pods"`
	OpenPods       int               `json:"open_pods"`
	PerRunPeakPods ResourceQuantiles `json:"per_run_peak_pods"`
}

func resourceMetric(rows []StepResourceUsage, scope string, use, request func(StepResourceUsage) *int64) ResourceMetricReport {
	var used, requested, ratios []int64
	for _, v := range rows {
		u := use(v)
		if u != nil {
			used = append(used, *u)
		}
		if request != nil {
			if q := request(v); q != nil {
				requested = append(requested, *q)
				if u != nil && *q > 0 {
					ratio, _ := ResourceRate(*u, time.Duration(*q))
					ratios = append(ratios, ratio/1000)
				}
			}
		}
	}
	return ResourceMetricReport{Scope: scope, Used: Quantiles(used), Requested: Quantiles(requested), UsedRequestPPM: Quantiles(ratios)}
}

func BuildContainerResourceReport(values []StepResourceUsage) []ResourceContainerReport {
	groups := map[string][]StepResourceUsage{}
	for _, v := range values {
		key := strings.Join([]string{v.Stage, v.Step, v.EffectivePodKind(), v.EffectiveContainerKind(), v.ContainerName}, "\x00")
		groups[key] = append(groups[key], v)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]ResourceContainerReport, 0, len(keys))
	for _, key := range keys {
		rows := groups[key]
		v := rows[0]
		r := ResourceContainerReport{Stage: v.Stage, Step: v.Step, ContainerName: v.ContainerName, ContainerKind: v.EffectiveContainerKind(), PodKind: v.EffectivePodKind(), Observations: len(rows), Metrics: map[string]ResourceMetricReport{}}
		for _, row := range rows {
			if row.Samples == 0 {
				r.Unsampled++
			}
			if row.Reason == "OOMKilled" {
				r.OOMKilled++
			}
			if row.Saturated || row.CounterReset || row.RequestsChanged || row.SizeClassChanged {
				r.Flagged++
			}
		}
		for _, m := range []struct {
			name         string
			use, request func(StepResourceUsage) *int64
		}{
			{"cpu_nanocores", func(v StepResourceUsage) *int64 { return v.CPUPeakNanoCores }, func(v StepResourceUsage) *int64 { return v.CPURequestNanoCores }},
			{"memory_bytes", func(v StepResourceUsage) *int64 { return v.MemoryPeakBytes }, func(v StepResourceUsage) *int64 { return v.MemoryRequestBytes }},
			{"rootfs_bytes", func(v StepResourceUsage) *int64 { return v.RootfsPeakBytes }, nil},
			{"logs_bytes", func(v StepResourceUsage) *int64 { return v.LogsPeakBytes }, nil},
			{"io_read_bytes", func(v StepResourceUsage) *int64 { return v.IOReadBytes }, nil},
			{"io_write_bytes", func(v StepResourceUsage) *int64 { return v.IOWriteBytes }, nil},
			{"io_read_ops", func(v StepResourceUsage) *int64 { return v.IOReadOps }, nil},
			{"io_write_ops", func(v StepResourceUsage) *int64 { return v.IOWriteOps }, nil},
		} {
			r.Metrics[m.name] = resourceMetric(rows, "container", m.use, m.request)
		}
		out = append(out, r)
	}
	return out
}

func BuildPodResourceReport(values []StepResourceUsage) ([]ResourcePodReport, ResourceOverlapReport) {
	// Pod-scoped counters are repeated in container rows. Deduplicate by UID
	// before computing distributions or overlap; never add these repetitions.
	pods := map[string]StepResourceUsage{}
	ends := map[string]time.Time{}
	for _, v := range values {
		key := v.RunID + "\x00" + v.PodUID
		end := v.ContainerFinishedAt
		if !v.Terminal && !v.ContainerStartedAt.IsZero() {
			end = v.LastObservedAt
		}
		if end.After(ends[key]) {
			ends[key] = end
		}
		if old, ok := pods[key]; ok {
			if v.LastObservedAt.After(old.LastObservedAt) {
				old.LastObservedAt = v.LastObservedAt
			}
			if old.PodStartedAt.IsZero() {
				old.PodStartedAt = v.PodStartedAt
			}
			if old.PodFinishedAt.IsZero() {
				old.PodFinishedAt = v.PodFinishedAt
			}
			for _, pair := range []struct {
				dst **int64
				src *int64
			}{{&old.EphemeralStoragePeakBytes, v.EphemeralStoragePeakBytes}, {&old.PodPVCUsedPeakBytes, v.PodPVCUsedPeakBytes}, {&old.PodCPUPeakNanoCores, v.PodCPUPeakNanoCores}, {&old.PodMemoryPeakBytes, v.PodMemoryPeakBytes}} {
				if pair.src != nil && (*pair.dst == nil || *pair.src > **pair.dst) {
					*pair.dst = pair.src
				}
			}
			pods[key] = old
		} else {
			pods[key] = v
		}
	}
	groups := map[string][]StepResourceUsage{}
	type event struct {
		at    time.Time
		delta int
	}
	events := map[string][]event{}
	overlap := ResourceOverlapReport{Semantics: "per-run maximum overlapping kubelet pod lifetimes in selected rows; half-open intervals; incomplete lifetimes end at last observed running container or known container finish; missing/truncated/GC'd pods remain unknown; no sum of independent resource peaks", Pods: len(pods)}
	for podKey, v := range pods {
		key := v.Stage + "\x00" + v.Step + "\x00" + v.EffectivePodKind()
		groups[key] = append(groups[key], v)
		if v.PodStartedAt.IsZero() {
			continue
		}
		end := v.PodFinishedAt
		if end.IsZero() {
			end = ends[podKey]
			overlap.OpenPods++
		}
		if !end.After(v.PodStartedAt) {
			continue
		}
		overlap.TimedPods++
		events[v.RunID] = append(events[v.RunID], event{v.PodStartedAt, 1}, event{end, -1})
	}
	var peaks []int64
	for _, runEvents := range events {
		slices.SortFunc(runEvents, func(a, b event) int {
			if n := a.at.Compare(b.at); n != 0 {
				return n
			}
			return cmp.Compare(a.delta, b.delta)
		})
		current, peak := 0, 0
		for _, e := range runEvents {
			current += e.delta
			peak = max(peak, current)
		}
		peaks = append(peaks, int64(peak))
	}
	overlap.PerRunPeakPods = Quantiles(peaks)
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]ResourcePodReport, 0, len(keys))
	for _, key := range keys {
		rows := groups[key]
		v := rows[0]
		var ephemeral, pvc, cpu, memory []int64
		for _, row := range rows {
			if row.PodCPUPeakNanoCores != nil {
				cpu = append(cpu, *row.PodCPUPeakNanoCores)
			}
			if row.PodMemoryPeakBytes != nil {
				memory = append(memory, *row.PodMemoryPeakBytes)
			}
			if row.EphemeralStoragePeakBytes != nil {
				ephemeral = append(ephemeral, *row.EphemeralStoragePeakBytes)
			}
			if row.PodPVCUsedPeakBytes != nil {
				pvc = append(pvc, *row.PodPVCUsedPeakBytes)
			}
		}
		out = append(out, ResourcePodReport{Stage: v.Stage, Step: v.Step, PodKind: v.EffectivePodKind(), Pods: len(rows), CPUPeakNanoCores: Quantiles(cpu), MemoryPeakBytes: Quantiles(memory), EphemeralStoragePeakBytes: Quantiles(ephemeral), PVCUsedPeakBytes: Quantiles(pvc)})
	}
	return out, overlap
}
