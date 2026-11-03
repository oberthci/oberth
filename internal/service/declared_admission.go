package service

import (
	"errors"
	"fmt"
	"maps"
	"strings"

	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// DeclaredAdmissionConfig is administrator-owned. Disabled is the single kill
// switch for new declared work; repositories opt in by committing declarations.
type DeclaredAdmissionConfig struct {
	// LegacyResources overrides fixed CPU/memory charges per size. Missing sizes
	// retain defaults independently of host budgets and size weights.
	LegacyResources   map[pipeline.Size]model.LegacyResourceCharge
	ExtendedResources map[string]int64
	Disabled          bool
	CPUMilli          int64
	MemoryBytes       int64
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
	c.LegacyResources = maps.Clone(c.LegacyResources)
	fixed, percent := c.memoryHeadroom()
	c.MemoryHeadroomBytes, c.MemoryHeadroomPercent = ptr.To(fixed), ptr.To(percent)
	return c
}

func (c DeclaredAdmissionConfig) memoryHeadroom() (fixed, percent int64) {
	return ptr.Deref(c.MemoryHeadroomBytes, 1<<30), ptr.Deref(c.MemoryHeadroomPercent, 10)
}

func (c DeclaredAdmissionConfig) legacyCharge(size pipeline.Size) model.LegacyResourceCharge {
	if charge, ok := c.LegacyResources[size.Effective()]; ok {
		return charge
	}
	switch size.Effective() {
	case pipeline.S:
		return model.LegacyResourceCharge{CPUMilli: 1000, MemoryBytes: 2 << 30}
	case pipeline.L:
		return model.LegacyResourceCharge{CPUMilli: 3000, MemoryBytes: 8 << 30}
	case pipeline.XL:
		return model.LegacyResourceCharge{CPUMilli: 6000, MemoryBytes: 16 << 30}
	default:
		return model.LegacyResourceCharge{CPUMilli: 2000, MemoryBytes: 4 << 30}
	}
}

// effectiveLegacyCharge bounds an exclusive XL reservation to the host budget.
// Shared sizes keep their fixed charge, even when an adopted grant no longer fits.
func (c DeclaredAdmissionConfig) effectiveLegacyCharge(size pipeline.Size, weights SizeWeights) model.LegacyResourceCharge {
	charge := c.legacyCharge(size)
	if weights.wholeBudget(size) {
		charge.CPUMilli = min(charge.CPUMilli, c.CPUMilli)
		charge.MemoryBytes = min(charge.MemoryBytes, c.MemoryBytes)
	}
	return charge
}

func (c DeclaredAdmissionConfig) Validate() error {
	return c.ValidateForSizeWeights(DefaultSizeWeights)
}

// ValidateForSizeWeights checks configured charges against the budget using
// the same exclusive XL policy as admission. Omitted sizes retain defaults.
func (c DeclaredAdmissionConfig) ValidateForSizeWeights(weights SizeWeights) error {
	for size, charge := range c.LegacyResources {
		if size == "" || !size.Valid() || charge.CPUMilli <= 0 || charge.CPUMilli > 1_000_000 || charge.MemoryBytes <= 0 || charge.MemoryBytes > 1<<50 {
			return errors.New("legacy admission charges require sizes S, M, L or XL, CPU in 1..1000000 millis and memory in 1..2^50 bytes")
		}
	}
	if err := argoworkflow.ValidateExtendedResourceLimits(c.ExtendedResources); err != nil {
		return err
	}
	fixed, percent := c.memoryHeadroom()
	if c.CPUMilli <= 0 || c.CPUMilli > 1_000_000 || c.MemoryBytes <= 0 || c.MemoryBytes > 1<<50 || fixed < 0 || fixed > 1<<50 || percent < 0 || percent > 100 || c.IORuns < 0 || c.IORuns > 64 {
		return errors.New("declared admission requires positive CPU/memory budgets, memory headroom in 0..2^50 bytes and 0..100 percent and an IO run ceiling in 0..64")
	}
	for _, size := range []pipeline.Size{pipeline.S, pipeline.M, pipeline.L, pipeline.XL} {
		if _, configured := c.LegacyResources[size]; !configured {
			continue
		}
		charge := c.effectiveLegacyCharge(size, weights)
		if charge.CPUMilli > c.CPUMilli {
			return fmt.Errorf("legacy admission charge %s CPU %dm exceeds numeric CPU budget %dm", size, charge.CPUMilli, c.CPUMilli)
		}
		if charge.MemoryBytes > c.MemoryBytes {
			return fmt.Errorf("legacy admission charge %s memory %d bytes exceeds numeric memory budget %d bytes", size, charge.MemoryBytes, c.MemoryBytes)
		}
	}
	return nil
}

func (g *weightedAdmission) legacyCharge(size pipeline.Size) model.LegacyResourceCharge {
	return g.resources.effectiveLegacyCharge(size, g.weights)
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
	if g.workflowOnly {
		return true
	}
	cpu, memory := g.resources.CPUMilli, g.resources.MemoryBytes
	extended := maps.Clone(g.resources.ExtendedResources)
	charge := func(r *admissionReservation) bool {
		legacy := g.legacyCharge(r.size)
		d := argoworkflow.PodDemand{CPUMilli: legacy.CPUMilli, MemoryBytes: legacy.MemoryBytes}
		if g.usesDeclarations(r) {
			d = g.runCharge(*r.pods)
		}
		for name, count := range d.ExtendedResources {
			if count > extended[name] {
				return false
			}
			extended[name] -= count
		}
		if d.CPUMilli > cpu || d.MemoryBytes > memory {
			return false
		}
		cpu -= d.CPUMilli
		memory -= d.MemoryBytes
		return true
	}
	for _, r := range g.reservations {
		if r.granted && !r.released {
			if !charge(r) {
				return false
			}
		}
	}
	return charge(candidate)
}

func (g *weightedAdmission) numericBudgetLocked() *model.NumericBudget {
	if g.workflowOnly {
		return nil
	}
	b := &model.NumericBudget{CPUBudgetMilli: g.resources.CPUMilli, MemoryBudgetBytes: g.resources.MemoryBytes}
	for _, r := range g.reservations {
		if !r.granted || r.released {
			continue
		}
		if r.pods != nil {
			d := g.runCharge(*r.pods)
			b.CPUUsedMilli += d.CPUMilli
			b.MemoryUsedBytes += d.MemoryBytes
		} else {
			charge := g.legacyCharge(r.size)
			b.CPUUsedMilli += charge.CPUMilli
			b.MemoryUsedBytes += charge.MemoryBytes
		}
	}
	return b
}

func (g *weightedAdmission) numericBlockedLocked(r *admissionReservation) bool {
	if g.workflowOnly {
		return false
	}
	return (r.pods != nil || !g.weights.wholeBudget(r.size)) && g.used+r.weight <= g.capacity && !g.numericFitsLocked(r)
}

func (g *weightedAdmission) validatePodDemand(d *argoworkflow.PodDemand) error {
	if g.workflowOnly {
		return nil
	}
	if d == nil {
		return nil
	}
	if !d.Valid() {
		return errors.New("invalid declared workflow resource demand")
	}
	if g.resources.Disabled {
		return errors.New("declared resource admission is disabled; declared workflows cannot use deprecated size admission")
	}
	if !g.demandFitsBudget(d) {
		return errors.New("declared resource demand including helper and memory headroom reserves exceeds the numeric budget; reduce requests or parallelism in git, or raise the administrator budget")
	}
	return nil
}

func (g *weightedAdmission) demandFitsBudget(d *argoworkflow.PodDemand) bool {
	c := g.runCharge(*d)
	for name, count := range c.ExtendedResources {
		if count > g.resources.ExtendedResources[name] {
			return false
		}
	}
	return c.CPUMilli <= g.resources.CPUMilli && c.MemoryBytes <= g.resources.MemoryBytes
}

func (g *weightedAdmission) selectDeclaredDemand(receipt *argoworkflow.DemandReceipt) (*argoworkflow.PodDemand, model.ResourceAdmissionFallback, error) {
	if g.workflowOnly {
		return nil, "", nil
	}
	if receipt == nil {
		return nil, model.ResourceFallbackRequests, nil
	}
	d, err := receipt.DeclaredPodDemand()
	if d == nil && !receipt.HasResourceRequests() {
		return nil, model.ResourceFallbackRequests, nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("declared resource admission refused: %w; complete the declarations or bound workflow parallelism in git", err)
	}
	if d == nil {
		return nil, "", errors.New("declared resource admission refused: incomplete CPU/memory requests; complete every container declaration in git")
	}
	if err := g.validatePodDemand(d); err != nil {
		return nil, "", err
	}
	return d, "", nil
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
			message := ""
			if !g.workflowOnly {
				message = r.fallback.Message()
			}
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
	return a.Equal(*b)
}

// legacyResources distinguishes fixed size charges from declared Pod demand.
// An unsized claim is still preparing and must not look like a legacy run.
func (r *admissionReservation) legacyResources() *model.LegacyResourceCharge {
	if !r.prepared || r.pods != nil {
		return nil
	}
	charge := r.gate.legacyCharge(r.size)
	return &charge
}

func (r *admissionReservation) declaredResources() *model.DeclaredResources {
	if r.pods == nil {
		return nil
	}
	d := r.gate.runCharge(*r.pods)
	return &model.DeclaredResources{
		DeclaredTotal: r.pods.DeclaredTotal.Clone(),
		Peak: &argoworkflow.PodRequests{
			ExtendedResources: maps.Clone(r.pods.ExtendedResources),
			CPUMilli:          r.pods.CPUMilli, MemoryBytes: r.pods.MemoryBytes, Pods: r.pods.Pods,
		},
		ExtendedResources: maps.Clone(d.ExtendedResources),
		CPUMilli:          d.CPUMilli, MemoryBytes: d.MemoryBytes, Pods: d.Pods,
	}
}
