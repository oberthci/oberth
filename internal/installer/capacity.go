package installer

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// CapacityEvidence is a frozen, operator-reviewed sizing input. Receipt names
// identify external qualification artifacts; their presence does not authenticate
// them or establish that a production environment has passed qualification.
type CapacityEvidence struct {
	Snapshot             string                 `json:"snapshot"`
	CapturedAt           time.Time              `json:"capturedAt"`
	CurrentCap           int                    `json:"currentCap"`
	Origin               string                 `json:"origin"`
	Host                 map[string]any         `json:"host"`
	Node                 corev1.Node            `json:"node"`
	NonRunAndMaintenance []corev1.Pod           `json:"nonRunAndMaintenance"`
	Quotas               []corev1.ResourceQuota `json:"quotas"`
	OperatingScratch     resource.Quantity      `json:"operatingScratch"`
	Families             []CapacityFamily       `json:"families"`
	Receipts             map[string]string      `json:"receipts"`
}

// CapacityFamily retains concurrent final Pods, including all generated children.
// Intervals exclude zero-Pod lock waiters; Seconds weights active execution time.
type CapacityFamily struct {
	Name          string             `json:"name"`
	MixWeight     int                `json:"mixWeight"`
	Executions    int                `json:"executions"`
	JoinedPods    int                `json:"joinedPods"`
	SampledPods   int                `json:"sampledPods"`
	UnknownShapes bool               `json:"unknownShapes"`
	Intervals     []CapacityInterval `json:"intervals"`
	WorstPods     []corev1.Pod       `json:"worstPods"`
}

// CapacityInterval is one measured interval with nonterminal Pods present.
type CapacityInterval struct {
	Seconds int          `json:"seconds"`
	Pods    []corev1.Pod `json:"pods"`
}

// CapacityAdvice proposes a trial; nil Parallel means no recommendation. It never
// changes the chosen native cap or authorizes activation of a namespace policy.
type CapacityAdvice struct {
	Snapshot   string         `json:"snapshot"`
	CapturedAt time.Time      `json:"capturedAt"`
	CurrentCap int            `json:"currentCap"`
	Origin     string         `json:"origin"`
	Parallel   *int           `json:"parallel,omitempty"`
	Reason     string         `json:"reason"`
	Remedy     string         `json:"remedy"`
	Budget     [6]int64       `json:"budget"`
	Percentile [6]int64       `json:"percentile"`
	Mix        map[string]int `json:"mix"`
	Dimensions [6]string      `json:"dimensions"`
}

const capacityReason = "Insufficient capacity evidence."
const capacityRemedy = "Complete G1/G2/G3/G7, generated-Pod inventory and native placement/recovery receipts; review the frozen snapshot."

// AdviseCapacity is deterministic and read-only. Native effective Pod resource
// accounting supplies requests/limits; a percentile estimate still requires every
// supported worst shape to fit aggregate quota at the proposed concurrency.
func AdviseCapacity(e CapacityEvidence) CapacityAdvice {
	a := CapacityAdvice{Snapshot: e.Snapshot, CapturedAt: e.CapturedAt, CurrentCap: e.CurrentCap, Origin: e.Origin, Reason: capacityReason, Remedy: capacityRemedy}
	a.Mix = map[string]int{}
	a.Dimensions = [6]string{"cpuMilli", "memoryBytes", "ephemeralBytes", "pods", "memoryLimitBytes", "ephemeralLimitBytes"}
	for _, f := range e.Families {
		a.Mix[f.Name] = f.MixWeight
	}
	budget, ok := capacityBudget(e)
	if !ok {
		return a
	}
	samples, worst, ok := capacitySamples(e)
	if !ok {
		return a
	}
	a.Budget = budget
	cap, percentile := capacityProposal(budget, samples)
	a.Percentile = percentile
	if cap < 1 || cap == math.MaxInt32 {
		return a
	}
	if !capacityVectorFit(budget, worst, cap) || !capacityNamespaceFit(e, cap) {
		return a
	}
	parallel := int(cap)
	a.Parallel = &parallel
	a.Reason = "Capacity trial requires qualification."
	a.Remedy = "Keep the chosen cap until native replay and adjacent-cap throughput/health qualification pass."
	return a
}

func capacityProposal(budget [6]int64, samples []capacitySample) (int64, [6]int64) {
	cap := int64(math.MaxInt32)
	var percentile [6]int64
	for d := range 4 {
		percentile[d] = capacityPercentile(samples, d)
		if percentile[d] > 0 {
			cap = min(cap, budget[d]/percentile[d])
		}
	}
	return cap, percentile
}

func capacityVectorFit(budget, vector [6]int64, cap int64) bool {
	for d, n := range vector {
		if n > 0 && cap > budget[d]/n {
			return false
		}
	}
	return true
}

type capacitySample struct {
	vector [6]int64
	weight float64
}

func capacitySamples(e CapacityEvidence) ([]capacitySample, [6]int64, bool) {
	var samples []capacitySample
	var worst [6]int64
	seen := map[string]bool{}
	for _, family := range e.Families {
		if seen[family.Name] {
			return nil, worst, false
		}
		seen[family.Name] = true
		part, maximum, ok := capacityFamilySamples(e, family)
		if !ok {
			return nil, worst, false
		}
		for d, n := range maximum {
			worst[d] = max(worst[d], n)
		}
		samples = append(samples, part...)
	}
	return samples, worst, len(samples) > 0
}

func capacityFamilySamples(e CapacityEvidence, family CapacityFamily) ([]capacitySample, [6]int64, bool) {
	maximum, ok := runVector(e, family.WorstPods)
	if !ok || !capacityFamilyCoverage(family) {
		return nil, maximum, false
	}
	var seconds float64
	for _, interval := range family.Intervals {
		if interval.Seconds <= 0 {
			return nil, maximum, false
		}
		seconds += float64(interval.Seconds)
	}
	if seconds == 0 {
		return nil, maximum, false
	}
	var samples []capacitySample
	for _, interval := range family.Intervals {
		vector, valid := runVector(e, interval.Pods)
		if !valid {
			return nil, maximum, false
		}
		for d, n := range vector {
			if n > maximum[d] {
				return nil, maximum, false
			}
		}
		samples = append(samples, capacitySample{vector: vector, weight: float64(family.MixWeight) * float64(interval.Seconds) / seconds})
	}
	return samples, maximum, true
}

func capacityFamilyCoverage(f CapacityFamily) bool {
	return f.Name != "" && f.MixWeight > 0 && f.Executions >= 20 && !f.UnknownShapes && f.SampledPods > 0 && f.JoinedPods >= 0 && f.JoinedPods <= f.SampledPods && float64(f.JoinedPods)/float64(f.SampledPods) >= .99
}

func capacityPercentile(samples []capacitySample, dimension int) int64 {
	sort.Slice(samples, func(i, j int) bool { return samples[i].vector[dimension] < samples[j].vector[dimension] })
	var total, cumulative float64
	for _, sample := range samples {
		total += sample.weight
	}
	for _, sample := range samples {
		cumulative += sample.weight
		if cumulative >= .95*total {
			return sample.vector[dimension]
		}
	}
	return samples[len(samples)-1].vector[dimension]
}

// CapacityLines keeps unavailable advice separate from normal installation and
// push behavior. Full evidence and vectors remain available in JSON output.
func CapacityLines(a CapacityAdvice) []string {
	lines := []string{fmt.Sprintf("Snapshot: %s (%s)", a.Snapshot, a.CapturedAt.UTC().Format(time.RFC3339)), fmt.Sprintf("Chosen parallel runs: %d (%s)", a.CurrentCap, a.Origin), a.Reason, a.Remedy}
	mix, _ := json.Marshal(a.Mix)
	lines = append(lines, "Frozen family mix: "+string(mix))
	if a.Parallel != nil {
		lines = append(lines, fmt.Sprintf("Proposed trial parallel runs: %d", *a.Parallel))
	}
	return lines
}
