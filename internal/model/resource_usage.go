package model

import (
	"encoding/json"
	"errors"
	"math"
	"math/bits"
	"slices"
	"time"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// StepResourceUsage is an external kubelet/cAdvisor observation. It never
// consumes the step's termination message or changes admission. Peaks are
// sampled lower bounds, including when a step is killed between polls.
type StepResourceUsage struct {
	// WorkflowSize is the observed Workflow admission label, never a numeric
	// per-step resource budget or a termination-message claim. Legacy unknowns
	// remain empty. Conflicting observed labels permanently mark it unknown.
	WorkflowSize              periapsis.Size `json:"workflow_size,omitempty"`
	SizeClassChanged          bool           `json:"size_class_changed,omitempty"`
	Version                   int            `json:"version"`
	Source                    string         `json:"source"`
	RunID                     string         `json:"run_id"`
	WorkflowName              string         `json:"workflow_name"`
	WorkflowUID               string         `json:"workflow_uid"`
	Namespace                 string         `json:"namespace"`
	NodeID                    string         `json:"node_id"`
	PodName                   string         `json:"pod_name"`
	PodUID                    string         `json:"pod_uid"`
	NodeName                  string         `json:"node_name"`
	ContainerID               string         `json:"container_id"`
	ContainerName             string         `json:"container_name"`
	RestartCount              int32          `json:"restart_count"`
	Burn                      string         `json:"burn"`
	Step                      string         `json:"step"`
	FirstObservedAt           time.Time      `json:"first_observed_at"`
	LastObservedAt            time.Time      `json:"last_observed_at"`
	SourceAt                  time.Time      `json:"source_at"`
	ContainerSourceAt         time.Time      `json:"container_source_at"`
	IOPressureAt              time.Time      `json:"io_pressure_at"`
	Samples                   int64          `json:"samples"`
	SampledLowerBound         bool           `json:"sampled_lower_bound"`
	Terminal                  bool           `json:"terminal"`
	ExitCode                  *int32         `json:"exit_code"`
	Reason                    string         `json:"reason,omitempty"`
	CPURequestNanoCores       *int64         `json:"cpu_request_nanocores"`
	MemoryRequestBytes        *int64         `json:"memory_request_bytes"`
	EphemeralRequestBytes     *int64         `json:"ephemeral_request_bytes"`
	CPUTimeNS                 *int64         `json:"cpu_time_ns"`
	CPUTimeAt                 time.Time      `json:"cpu_time_at"`
	CPUPeakNanoCores          *int64         `json:"cpu_peak_nanocores"`
	MemoryPeakBytes           *int64         `json:"memory_peak_bytes"`
	EphemeralStoragePeakBytes *int64         `json:"ephemeral_storage_peak_bytes"`
	IOReadBytes               *int64         `json:"io_read_bytes"`
	IOWriteBytes              *int64         `json:"io_write_bytes"`
	IOReadOps                 *int64         `json:"io_read_ops"`
	IOWriteOps                *int64         `json:"io_write_ops"`
	IOPressureSomeNS          *int64         `json:"io_pressure_some_ns"`
	IOPressureFullNS          *int64         `json:"io_pressure_full_ns"`
	Saturated                 bool           `json:"saturated"`
	CounterReset              bool           `json:"counter_reset"`
	RequestsChanged           bool           `json:"requests_changed"`
}

func (u StepResourceUsage) Validate() error {
	bad := errors.New("invalid external step resource observation")
	if !u.WorkflowSize.Valid() || (u.SizeClassChanged && u.WorkflowSize != "") {
		return bad
	}
	if u.Version != 1 || u.Source != "kubelet-cadvisor" || !u.SampledLowerBound || !diagnosticRun.MatchString(u.RunID) || !diagnosticUID.MatchString(u.WorkflowUID) || !diagnosticUID.MatchString(u.PodUID) || !diagnosticCID.MatchString(u.ContainerID) || u.ContainerName != "main" || u.RestartCount < 0 || u.Samples < 0 || (u.Samples == 0 && !u.Terminal) || u.FirstObservedAt.IsZero() || u.LastObservedAt.Before(u.FirstObservedAt) || (u.Samples > 0 && u.SourceAt.IsZero()) || u.SourceAt.After(u.LastObservedAt.Add(2*time.Second)) {
		return bad
	}
	for _, s := range []string{u.WorkflowName, u.Namespace, u.NodeID, u.PodName, u.NodeName, u.Burn, u.Step} {
		if !diagnosticName.MatchString(s) {
			return bad
		}
	}
	if u.Terminal != (u.ExitCode != nil) || (u.ExitCode != nil && *u.ExitCode < 0) {
		return bad
	}
	switch u.Reason {
	case "":
		if u.Terminal {
			return bad
		}
	case "Completed", "Error", "OOMKilled", "other":
		if !u.Terminal {
			return bad
		}
	default:
		return bad
	}
	if u.CPUTimeNS != nil && (*u.CPUTimeNS < 0 || u.CPUTimeAt.IsZero() || u.CPUTimeAt.After(u.LastObservedAt.Add(2*time.Second))) {
		return bad
	}
	for _, at := range []time.Time{u.ContainerSourceAt, u.IOPressureAt} {
		if at.After(u.LastObservedAt.Add(2 * time.Second)) {
			return bad
		}
	}
	for _, n := range u.counters() {
		if n != nil && *n < 0 {
			return bad
		}
	}
	data, err := json.Marshal(u)
	if err != nil || len(data) > 8192 {
		return bad
	}
	return nil
}
func (u StepResourceUsage) counters() []*int64 {
	return []*int64{u.CPURequestNanoCores, u.MemoryRequestBytes, u.EphemeralRequestBytes, u.CPUPeakNanoCores, u.MemoryPeakBytes, u.EphemeralStoragePeakBytes, u.IOReadBytes, u.IOWriteBytes, u.IOReadOps, u.IOWriteOps, u.IOPressureSomeNS, u.IOPressureFullNS}
}

// MergeResourceUsage never sums repeated cumulative samples or erases known
// measurements with missing data. Container incarnations are separate records.
func MergeResourceUsage(old, next StepResourceUsage) (StepResourceUsage, error) {
	if old.Validate() != nil || next.Validate() != nil || old.RunID != next.RunID || old.WorkflowUID != next.WorkflowUID || old.Namespace != next.Namespace || old.WorkflowName != next.WorkflowName || old.NodeID != next.NodeID || old.PodUID != next.PodUID || old.PodName != next.PodName || old.NodeName != next.NodeName || old.ContainerID != next.ContainerID || old.RestartCount != next.RestartCount || old.Burn != next.Burn || old.Step != next.Step {
		return StepResourceUsage{}, errors.New("resource observation binding changed")
	}
	out := old
	out.SizeClassChanged = old.SizeClassChanged || next.SizeClassChanged ||
		(old.WorkflowSize != "" && next.WorkflowSize != "" && old.WorkflowSize != next.WorkflowSize)
	if out.SizeClassChanged {
		out.WorkflowSize = ""
	} else if out.WorkflowSize == "" {
		out.WorkflowSize = next.WorkflowSize
	}
	if next.CPUTimeNS != nil && next.CPUTimeAt.After(old.CPUTimeAt) {
		if old.CPUTimeNS != nil {
			if *next.CPUTimeNS < *old.CPUTimeNS {
				out.CounterReset = true
			} else if elapsed := next.CPUTimeAt.Sub(old.CPUTimeAt); elapsed >= time.Second {
				rate, saturated := ResourceRate(*next.CPUTimeNS-*old.CPUTimeNS, elapsed)
				out.Saturated = out.Saturated || saturated
				if next.CPUPeakNanoCores == nil || rate > *next.CPUPeakNanoCores {
					next.CPUPeakNanoCores = &rate
				}
			}
		}
		out.CPUTimeNS = next.CPUTimeNS
		out.CPUTimeAt = next.CPUTimeAt
	}
	if next.FirstObservedAt.Before(out.FirstObservedAt) {
		out.FirstObservedAt = next.FirstObservedAt
	}
	newContainer := next.ContainerSourceAt.After(old.ContainerSourceAt)
	newPSI := next.IOPressureAt.After(old.IOPressureAt)
	newer := next.SourceAt.After(old.SourceAt) || newContainer || newPSI || next.CPUTimeAt.After(old.CPUTimeAt)
	if next.SourceAt.After(old.SourceAt) {
		out.SourceAt = next.SourceAt
	}
	if newContainer {
		out.ContainerSourceAt = next.ContainerSourceAt
	}
	if newPSI {
		out.IOPressureAt = next.IOPressureAt
	}
	if newer {
		if out.Samples < math.MaxInt64 {
			out.Samples++
		}
	}
	oldValues, newValues := old.counters(), next.counters()
	targets := []**int64{&out.CPURequestNanoCores, &out.MemoryRequestBytes, &out.EphemeralRequestBytes, &out.CPUPeakNanoCores, &out.MemoryPeakBytes, &out.EphemeralStoragePeakBytes, &out.IOReadBytes, &out.IOWriteBytes, &out.IOReadOps, &out.IOWriteOps, &out.IOPressureSomeNS, &out.IOPressureFullNS}
	for i, p := range newValues {
		if p == nil {
			continue
		}
		if oldValues[i] != nil {
			if i < 3 && *p != *oldValues[i] {
				out.RequestsChanged = true
			}
			if ((i >= 6 && i <= 9 && newContainer) || (i >= 10 && newPSI)) && *p < *oldValues[i] {
				out.CounterReset = true
			}
		}
		if oldValues[i] == nil || *p > *oldValues[i] {
			v := *p
			*targets[i] = &v
		}
	}
	if next.Terminal && !out.Terminal {
		out.Terminal = true
		out.ExitCode = next.ExitCode
		out.Reason = next.Reason
	}
	out.Saturated = out.Saturated || next.Saturated
	out.CounterReset = out.CounterReset || next.CounterReset
	out.RequestsChanged = out.RequestsChanged || next.RequestsChanged
	// Summary and cAdvisor have independent clocks. A newer CPU/memory sample
	// must still merge when a cached pod summary has the larger SourceAt.
	if out != old && next.LastObservedAt.After(out.LastObservedAt) {
		out.LastObservedAt = next.LastObservedAt
	}
	return out, out.Validate()
}

type ResourceQuantiles struct {
	Count int    `json:"count"`
	P50   *int64 `json:"p50"`
	P95   *int64 `json:"p95"`
	Max   *int64 `json:"max"`
}

func Quantiles(values []int64) ResourceQuantiles {
	if len(values) == 0 {
		return ResourceQuantiles{}
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	p50, p95, maximum := sorted[(len(sorted)*50+99)/100-1], sorted[(len(sorted)*95+99)/100-1], sorted[len(sorted)-1]
	return ResourceQuantiles{len(sorted), &p50, &p95, &maximum}
}

// ResourceRate converts cumulative CPU nanoseconds to nanocores without overflow.
func ResourceRate(delta int64, elapsed time.Duration) (int64, bool) {
	if delta < 0 || elapsed <= 0 {
		return 0, true
	}
	hi, lo := bits.Mul64(uint64(delta), 1000000000)
	if hi >= uint64(elapsed) {
		return math.MaxInt64, true
	}
	q, _ := bits.Div64(hi, lo, uint64(elapsed))
	if q > math.MaxInt64 {
		return math.MaxInt64, true
	}
	return int64(q), false
}
