package service

import (
	"testing"

	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestNumericBudgetReportsRunCharge(t *testing.T) {
	for _, test := range []struct {
		name           string
		fixed, percent *int64
	}{
		{name: "default headroom"},
		{name: "explicit zero", fixed: ptr.To(int64(0)), percent: ptr.To(int64(0))},
		{name: "fixed headroom", fixed: ptr.To(int64(2 << 30)), percent: ptr.To(int64(0))},
		{name: "proportional headroom", fixed: ptr.To(int64(0)), percent: ptr.To(int64(75))},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := newWeightedAdmission(8)
			g.resources.MemoryHeadroomBytes, g.resources.MemoryHeadroomPercent = test.fixed, test.percent
			demand := argoworkflow.PodDemand{CPUMilli: 4900, MemoryBytes: 3 << 30, Pods: 2}
			charge := g.runCharge(demand)
			reservation := g.reserve(1)
			release := acquireRequestWithinBudget(t, reservation, admissionRequest{Size: pipeline.XL, Pods: &demand})
			defer release()
			g.mu.Lock()
			budget := g.numericBudgetLocked()
			reported := reservation.declaredResources()
			// A candidate fits exactly when its charge fits the reported remainder.
			g.resources.CPUMilli, g.resources.MemoryBytes = 2*charge.CPUMilli, 2*charge.MemoryBytes
			candidate := &admissionReservation{pods: &demand}
			fits := g.numericFitsLocked(candidate)
			g.resources.MemoryBytes--
			over := g.numericFitsLocked(candidate)
			g.mu.Unlock()
			if budget.CPUUsedMilli != charge.CPUMilli || budget.MemoryUsedBytes != charge.MemoryBytes {
				t.Fatalf("reported usage = %dm/%d bytes, charge = %dm/%d bytes", budget.CPUUsedMilli, budget.MemoryUsedBytes, charge.CPUMilli, charge.MemoryBytes)
			}
			if reported.CPUMilli != charge.CPUMilli || reported.MemoryBytes != charge.MemoryBytes {
				t.Fatalf("reported reservation = %+v, charge = %+v", reported, charge)
			}
			if !fits || over {
				t.Fatalf("reported charge and admission disagree: exact=%t over=%t", fits, over)
			}
		})
	}
}
