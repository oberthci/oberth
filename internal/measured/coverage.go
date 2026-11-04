package measured

import (
	"math"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/internal/model"
)

const MinimumMetricSamples int64 = 4
const ProvisionalAdvice = "Provisional resource bounds, not a safe cold-run peak. Cache warmth and workload equivalence across revisions are unknown. Requests and limits retain captured historical/current floors; sampled headroom is advisory. Remeasure after workload, mount, cache or CPU-cap changes."

type resourceBaseline struct {
	cpu, memory, storage, memoryLimit, storageLimit int64
	unknown, unlimited, mixedStorageScope           bool
}

func (b *resourceBaseline) observe(o Observation) {
	v := o.Usage
	for _, p := range []struct {
		dst *int64
		src *int64
	}{
		{&b.cpu, v.CPURequestNanoCores}, {&b.memory, v.MemoryRequestBytes}, {&b.storage, v.EphemeralRequestBytes},
		{&b.memoryLimit, o.MemoryLimitBytes}, {&b.storageLimit, o.EphemeralLimitBytes},
	} {
		if p.src == nil {
			b.unknown = true
		} else {
			*p.dst = max(*p.dst, *p.src)
		}
	}
	if (o.MemoryLimitBytes != nil && *o.MemoryLimitBytes == 0) || (o.EphemeralLimitBytes != nil && *o.EphemeralLimitBytes == 0) {
		b.unlimited = true
	}
	if v.PodEphemeralLimitBytes == nil {
		b.unknown = true
	} else if o.EphemeralLimitBytes != nil && *v.PodEphemeralLimitBytes != *o.EphemeralLimitBytes {
		b.mixedStorageScope = true
	}
}

func (b resourceBaseline) apply(s *Suggested, d *Declaration) bool {
	if b.unknown || b.unlimited || b.mixedStorageScope {
		return false
	}
	s.CPUMilli = max(s.CPUMilli, b.cpu/1_000_000+boolInt(b.cpu%1_000_000 != 0))
	s.MemoryRequestBytes = max(s.MemoryRequestBytes, b.memory)
	s.EphemeralRequestBytes = max(s.EphemeralRequestBytes, b.storage)
	s.MemoryLimitBytes = max(s.MemoryLimitBytes, b.memoryLimit)
	s.EphemeralLimitBytes = max(s.EphemeralLimitBytes, b.storageLimit)
	if d != nil {
		for _, p := range []struct {
			section, name string
			dst           *int64
			cpu, limit    bool
		}{
			{"requests", "cpu", &s.CPUMilli, true, false}, {"requests", "memory", &s.MemoryRequestBytes, false, false}, {"requests", "ephemeral-storage", &s.EphemeralRequestBytes, false, false},
			{"limits", "memory", &s.MemoryLimitBytes, false, true}, {"limits", "ephemeral-storage", &s.EphemeralLimitBytes, false, true},
		} {
			raw, exists := d.Resources[p.section][p.name]
			if !exists {
				if p.limit {
					return false
				}
				continue
			}
			q, err := resource.ParseQuantity(raw)
			ceiling := resource.NewQuantity(math.MaxInt64, resource.DecimalSI)
			if p.cpu {
				ceiling = resource.NewMilliQuantity(math.MaxInt64, resource.DecimalSI)
			}
			if err != nil || q.Sign() < 0 || q.Cmp(*ceiling) > 0 || (p.limit && q.IsZero()) {
				return false
			}
			n := q.Value()
			if p.cpu {
				n = q.MilliValue()
			}
			*p.dst = max(*p.dst, n)
		}
	}
	s.MemoryLimitBytes = max(s.MemoryLimitBytes, s.MemoryRequestBytes)
	s.EphemeralLimitBytes = max(s.EphemeralLimitBytes, s.EphemeralRequestBytes)
	return true
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

func coveredMemory(o Observation) bool {
	return o.StepMemoryPeakBytes != nil || o.Usage.MemoryCoverage.Through(o.Usage.ContainerFinishedAt, MinimumMetricSamples)
}
func coveredCPU(v model.StepResourceUsage) bool {
	return v.CPUCoverage.Through(v.ContainerFinishedAt, MinimumMetricSamples) && !v.CPUTimeAt.Before(v.ContainerFinishedAt)
}
func coveredStorage(v model.StepResourceUsage) bool {
	return !v.PodStartedAt.IsZero() && v.StorageCoverage.Through(v.PodFinishedAt, MinimumMetricSamples)
}
