package service

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// DeclaredAdmissionConfig is administrator-owned. Disabled is the single kill
// switch; repositories opt into numeric admission by committing declarations.
type DeclaredAdmissionConfig struct {
	Disabled    bool
	CPUMilli    int64
	MemoryBytes int64
	// Zero retains the existing coarse run ceiling. IO is not inferred from CPU.
	IORuns int
}

func DefaultDeclaredAdmissionConfig() DeclaredAdmissionConfig {
	return DeclaredAdmissionConfig{CPUMilli: 8000, MemoryBytes: 16 << 30}
}

func (c DeclaredAdmissionConfig) Validate() error {
	if c.CPUMilli <= 0 || c.CPUMilli > 1_000_000 || c.MemoryBytes <= 0 || c.MemoryBytes > 1<<50 || c.IORuns < 0 || c.IORuns > 64 {
		return errors.New("declared admission requires positive CPU/memory budgets and an IO run ceiling in 0..64")
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

func (g *weightedAdmission) usesDeclarations(r *admissionReservation) bool {
	return r.pods != nil
}

func (g *weightedAdmission) numericFitsLocked(candidate *admissionReservation) bool {
	cpu, memory := g.resources.CPUMilli, g.resources.MemoryBytes
	legacy := 0
	charge := func(r *admissionReservation) bool {
		if g.usesDeclarations(r) {
			d := podCharge(*r.pods)
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

func (g *weightedAdmission) validatePodDemand(d *argoworkflow.PodDemand) error {
	if d == nil || g.resources.Disabled {
		return nil
	}
	if !d.Valid() {
		return errors.New("invalid declared workflow resource demand")
	}
	c := podCharge(*d)
	if c.CPUMilli > g.resources.CPUMilli || c.MemoryBytes > g.resources.MemoryBytes {
		return fmt.Errorf("declared workflow needs %dm CPU and %d memory bytes including executor reserve; administrator budget is %dm CPU and %d memory bytes", c.CPUMilli, c.MemoryBytes, g.resources.CPUMilli, g.resources.MemoryBytes)
	}
	return nil
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
	d := podCharge(*r.pods)
	return &model.DeclaredResources{CPUMilli: d.CPUMilli, MemoryBytes: d.MemoryBytes, Pods: d.Pods}
}
