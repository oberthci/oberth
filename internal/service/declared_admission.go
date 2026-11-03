package service

import (
	"errors"
	"math/big"
	"strings"

	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// DeclaredAdmissionConfig is administrator-owned. Disabled is the single kill
// switch; repositories opt into numeric admission by committing declarations.
type DeclaredAdmissionConfig struct {
	Disabled    bool
	CPUMilli    int64
	MemoryBytes int64
	// Headroom is max(fixed bytes, percentage of declared run memory). Nil
	// selects the default independently of CPU/memory budgets; zero is explicit.
	MemoryHeadroomBytes   *int64
	MemoryHeadroomPercent *int64
	// Zero retains the existing coarse run ceiling. IO is not inferred from CPU.
	IORuns int
}

func DefaultDeclaredAdmissionConfig() DeclaredAdmissionConfig {
	return DeclaredAdmissionConfig{CPUMilli: 8000, MemoryBytes: 16 << 30, MemoryHeadroomBytes: ptr.To(int64(1 << 30)), MemoryHeadroomPercent: ptr.To(int64(10))}
}

// withHeadroomDefaults also copies optional values so callers cannot mutate an
// active scheduler's accounting through their original configuration pointers.
func (c DeclaredAdmissionConfig) withHeadroomDefaults() DeclaredAdmissionConfig {
	fixed, percent := c.memoryHeadroom()
	c.MemoryHeadroomBytes, c.MemoryHeadroomPercent = ptr.To(fixed), ptr.To(percent)
	return c
}

func (c DeclaredAdmissionConfig) memoryHeadroom() (fixed, percent int64) {
	return ptr.Deref(c.MemoryHeadroomBytes, 1<<30), ptr.Deref(c.MemoryHeadroomPercent, 10)
}

func (c DeclaredAdmissionConfig) Validate() error {
	fixed, percent := c.memoryHeadroom()
	if c.CPUMilli <= 0 || c.CPUMilli > 1_000_000 || c.MemoryBytes <= 0 || c.MemoryBytes > 1<<50 || fixed < 0 || fixed > 1<<50 || percent < 0 || percent > 100 || c.IORuns < 0 || c.IORuns > 64 {
		return errors.New("declared admission requires positive CPU/memory budgets, memory headroom in 0..2^50 bytes and 0..100 percent and an IO run ceiling in 0..64")
	}
	return nil
}

// Hold additional capacity for executor/init helpers without changing any Pod
// declaration. These fixed reserves must cover the deployment's executor
// defaults; they are not usage estimates or automatic request mutations.
const executorCPUMilli int64 = 100
const executorMemoryBytes int64 = 256 << 20

func podCharge(d argoworkflow.PodDemand) argoworkflow.PodDemand {
	d.CPUMilli += d.Pods * executorCPUMilli
	d.MemoryBytes += d.Pods * executorMemoryBytes
	return d
}

// runCharge holds max(fixed, proportional) overshoot headroom once per run.
// The percentage uses the raw request/VM envelope, excluding executor reserves.
// Kubernetes still schedules pods by their actual requests.
func (g *weightedAdmission) runCharge(d argoworkflow.PodDemand) argoworkflow.PodDemand {
	fixed, percent := g.resources.memoryHeadroom()
	// Round up to whole bytes without multiplying the full demand first. For
	// valid demand <= 2^60 and percent <= 100, all intermediates fit in int64.
	proportional := d.MemoryBytes/100*percent + (d.MemoryBytes%100*percent+99)/100
	d = podCharge(d)
	d.MemoryBytes += max(fixed, proportional)
	return d
}

func (g *weightedAdmission) usesDeclarations(r *admissionReservation) bool {
	return r.pods != nil
}

func (g *weightedAdmission) numericFitsLocked(candidate *admissionReservation) bool {
	cpu, memory := g.resources.CPUMilli, g.resources.MemoryBytes
	legacy := 0
	charge := func(r *admissionReservation) bool {
		if g.usesDeclarations(r) {
			d := g.runCharge(*r.pods)
			if d.CPUMilli > cpu || d.MemoryBytes > memory {
				return false
			}
			cpu -= d.CPUMilli
			memory -= d.MemoryBytes
		} else {
			if r.weight > g.capacity-legacy {
				return false
			}
			legacy += r.weight
		}
		return true
	}
	for _, r := range g.reservations {
		if r.granted && !r.released {
			if !charge(r) {
				return false
			}
		}
	}
	if !charge(candidate) {
		return false
	}
	// Legacy weights reserve the same fraction of BOTH numeric budgets. Round
	// once for the combined fraction, so M runs still exactly fill their budget.
	fits := func(available, budget int64) bool {
		left := new(big.Int).Mul(big.NewInt(available), big.NewInt(int64(g.capacity)))
		right := new(big.Int).Mul(big.NewInt(budget), big.NewInt(int64(legacy)))
		return left.Cmp(right) >= 0
	}
	return fits(cpu, g.resources.CPUMilli) && fits(memory, g.resources.MemoryBytes)
}

func (g *weightedAdmission) numericBudgetLocked() *model.NumericBudget {
	b := &model.NumericBudget{CPUBudgetMilli: g.resources.CPUMilli, MemoryBudgetBytes: g.resources.MemoryBytes}
	legacy := int64(0)
	for _, r := range g.reservations {
		if !r.granted || r.released {
			continue
		}
		if r.pods != nil {
			d := g.runCharge(*r.pods)
			b.CPUUsedMilli += d.CPUMilli
			b.MemoryUsedBytes += d.MemoryBytes
		} else {
			legacy += int64(r.weight)
		}
	}
	share := func(budget int64) int64 {
		// Ceil the combined legacy fraction once, exactly as numericFitsLocked.
		n := new(big.Int).Mul(big.NewInt(budget), big.NewInt(legacy))
		n.Add(n, big.NewInt(int64(g.capacity)-1))
		return n.Quo(n, big.NewInt(int64(g.capacity))).Int64()
	}
	b.CPUUsedMilli += share(b.CPUBudgetMilli)
	b.MemoryUsedBytes += share(b.MemoryBudgetBytes)
	return b
}

func (g *weightedAdmission) numericBlockedLocked(r *admissionReservation) bool {
	return (r.pods != nil || !g.weights.wholeBudget(r.size)) && g.used+r.weight <= g.capacity && !g.numericFitsLocked(r)
}

func (g *weightedAdmission) validatePodDemand(d *argoworkflow.PodDemand) error {
	if d == nil || g.resources.Disabled {
		return nil
	}
	if !d.Valid() {
		return errors.New("invalid declared workflow resource demand")
	}
	return nil
}

func (g *weightedAdmission) demandFitsBudget(d *argoworkflow.PodDemand) bool {
	c := g.runCharge(*d)
	return c.CPUMilli <= g.resources.CPUMilli && c.MemoryBytes <= g.resources.MemoryBytes
}

func (g *weightedAdmission) selectDeclaredDemand(receipt *argoworkflow.VMIDemandReceipt) (*argoworkflow.PodDemand, model.ResourceAdmissionFallback) {
	if g.resources.Disabled || receipt == nil {
		return nil, ""
	}
	d, err := receipt.DeclaredPodDemand()
	if errors.Is(err, argoworkflow.ErrMemoryUnbounded) {
		return nil, model.ResourceFallbackMemory
	}
	if errors.Is(err, argoworkflow.ErrVMHostUndeclared) {
		return nil, model.ResourceFallbackVM
	}
	if err != nil {
		return nil, model.ResourceFallbackGraph
	}
	if d == nil {
		return nil, model.ResourceFallbackRequests
	}
	if !d.Valid() {
		return nil, model.ResourceFallbackGraph
	}
	if !g.demandFitsBudget(d) {
		return nil, model.ResourceFallbackBudget
	}
	return d, ""
}

// Only called after the API has authorized the run. This also works before a
// Workflow exists; completed notices come from immutable timing metadata.
func (s *Scheduler) ResourceAdmissionMessage(runID string) string {
	if s == nil || s.admission == nil {
		return ""
	}
	g := s.admission
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.reservations {
		if r.runID == runID {
			message := r.fallback.Message()
			if r.pods != nil {
				message = strings.TrimSpace(message + " " + r.pods.ConcurrencyMessage())
			}
			return message
		}
	}
	return ""
}

func samePodDemand(a, b *argoworkflow.PodDemand) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (r *admissionReservation) declaredResources() *model.DeclaredResources {
	if r.pods == nil {
		return nil
	}
	d := r.gate.runCharge(*r.pods)
	return &model.DeclaredResources{CPUMilli: d.CPUMilli, MemoryBytes: d.MemoryBytes, Pods: d.Pods}
}
