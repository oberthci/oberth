package measured

import (
	"github.com/oberthci/oberth/internal/model"
)

// CPURequestPercentile sizes scheduling shares from completed-run average CPU,
// not sampled peaks. Limits remain the burst ceiling. One run gets one vote;
// repeated invocations use that run's highest average for the template.
func CPURequestPercentile(history []model.StepResourceUsage) map[string]int64 {
	groups := map[string]map[string]int64{}
	for _, v := range history {
		if !v.Terminal || v.ExitCode == nil || *v.ExitCode != 0 || v.TemplateName == "" || v.RunID == "" || v.ContainerName != "main" || v.EffectiveContainerKind() != "app" || v.EffectivePodKind() != "workflow" || v.RestartCount != 0 || v.Saturated || v.CounterReset || v.RequestsChanged || v.CPUTimeNS == nil || *v.CPUTimeNS < 0 || v.ContainerStartedAt.IsZero() || !v.ContainerFinishedAt.After(v.ContainerStartedAt) || !coveredCPU(v) {
			continue
		}
		n, saturated := model.ResourceRate(*v.CPUTimeNS, v.ContainerFinishedAt.Sub(v.ContainerStartedAt))
		if saturated || n > 1_000_000_000_000 {
			continue
		}
		if groups[v.TemplateName] == nil {
			groups[v.TemplateName] = map[string]int64{}
		}
		groups[v.TemplateName][v.RunID] = max(groups[v.TemplateName][v.RunID], n)
	}
	out := map[string]int64{}
	for template, runs := range groups {
		if len(runs) < MinimumSamples {
			continue
		}
		values := make([]int64, 0, len(runs))
		for _, n := range runs {
			values = append(values, n)
		}
		q := model.Quantiles(values)
		// 25% margin, 10m granularity, 50m floor; cap and authored request are
		// applied separately so sizing can only lower a scheduling request.
		out[template] = roundMargin(*q.P95, RequestMarginPercent, 10_000_000, 50_000_000) / 1_000_000
	}
	return out
}
