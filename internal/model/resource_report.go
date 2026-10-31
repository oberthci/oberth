package model

import (
	"slices"
	"strings"
	"time"
)

type ResourceUsageReport struct {
	StepsTruncated          bool                 `json:"steps_truncated"`
	Repository              string               `json:"repository"`
	Since                   time.Time            `json:"since"`
	Limit                   int                  `json:"limit"`
	Observations            int                  `json:"observations"`
	Truncated               bool                 `json:"truncated"`
	PopulationCoverageKnown bool                 `json:"population_coverage_known"`
	PeakSemantics           string               `json:"peak_semantics"`
	Steps                   []ResourceStepReport `json:"steps"`
}
type ResourceStepReport struct {
	Burn         string                          `json:"burn"`
	Step         string                          `json:"step"`
	Observations int                             `json:"observations"`
	Terminal     int                             `json:"terminal"`
	Killed       int                             `json:"oom_killed"`
	Unsampled    int                             `json:"unsampled"`
	Flagged      int                             `json:"flagged"`
	Metrics      map[string]ResourceMetricReport `json:"metrics"`
}
type ResourceMetricReport struct {
	Scope     string            `json:"scope"`
	Used      ResourceQuantiles `json:"used"`
	Requested ResourceQuantiles `json:"requested"`
	// One million means 100%. Ratios pair each observation's used/requested;
	// an unknown measurement or zero request contributes no ratio sample.
	UsedRequestPPM ResourceQuantiles `json:"used_request_ppm"`
}

func BuildResourceReport(values []StepResourceUsage) []ResourceStepReport {
	grouped := map[string][]StepResourceUsage{}
	for _, v := range values {
		key := v.Burn + "\x00" + v.Step
		grouped[key] = append(grouped[key], v)
	}
	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]ResourceStepReport, 0, len(keys))
	for _, key := range keys {
		burn, step, _ := strings.Cut(key, "\x00")
		rows := grouped[key]
		r := ResourceStepReport{Burn: burn, Step: step, Observations: len(rows), Metrics: map[string]ResourceMetricReport{}}
		for _, v := range rows {
			if v.Terminal {
				r.Terminal++
			}
			if v.Reason == "OOMKilled" {
				r.Killed++
			}
			if v.Samples == 0 {
				r.Unsampled++
			}
			if v.Saturated || v.CounterReset || v.RequestsChanged {
				r.Flagged++
			}
		}
		for _, metric := range []struct {
			name, scope   string
			used, request func(StepResourceUsage) *int64
		}{
			{"cpu_nanocores", "main", func(v StepResourceUsage) *int64 { return v.CPUPeakNanoCores }, func(v StepResourceUsage) *int64 { return v.CPURequestNanoCores }},
			{"memory_bytes", "main", func(v StepResourceUsage) *int64 { return v.MemoryPeakBytes }, func(v StepResourceUsage) *int64 { return v.MemoryRequestBytes }},
			{"ephemeral_storage_bytes", "pod", func(v StepResourceUsage) *int64 { return v.EphemeralStoragePeakBytes }, func(v StepResourceUsage) *int64 { return v.EphemeralRequestBytes }},
			{"io_read_bytes", "main", func(v StepResourceUsage) *int64 { return v.IOReadBytes }, nil},
			{"io_write_bytes", "main", func(v StepResourceUsage) *int64 { return v.IOWriteBytes }, nil},
			{"io_read_ops", "main", func(v StepResourceUsage) *int64 { return v.IOReadOps }, nil},
			{"io_write_ops", "main", func(v StepResourceUsage) *int64 { return v.IOWriteOps }, nil},
			{"io_pressure_some_ns", "pod", func(v StepResourceUsage) *int64 { return v.IOPressureSomeNS }, nil},
			{"io_pressure_full_ns", "pod", func(v StepResourceUsage) *int64 { return v.IOPressureFullNS }, nil},
		} {
			used, requested, ratios := []int64{}, []int64{}, []int64{}
			for _, v := range rows {
				u := metric.used(v)
				if u != nil {
					used = append(used, *u)
				}
				if metric.request != nil {
					q := metric.request(v)
					if q != nil {
						requested = append(requested, *q)
						if u != nil && *q > 0 {
							ratio, _ := ResourceRate(*u, time.Duration(*q))
							ratios = append(ratios, ratio/1000)
						}
					}
				}
			}
			r.Metrics[metric.name] = ResourceMetricReport{Scope: metric.scope, Used: Quantiles(used), Requested: Quantiles(requested), UsedRequestPPM: Quantiles(ratios)}
		}
		out = append(out, r)
	}
	return out
}

func AttachResourceUsage(steps []StepResult, values []StepResourceUsage, truncated bool) {
	grouped := map[string][]StepResourceUsage{}
	for _, v := range values {
		key := v.Burn + "\x00" + v.Step
		grouped[key] = append(grouped[key], v)
	}
	for i := range steps {
		steps[i].MeasuredUsage = grouped[steps[i].Burn+"\x00"+steps[i].Step]
		steps[i].MeasuredUsageTruncated = truncated
	}
}
