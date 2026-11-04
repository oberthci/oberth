package argoworkflow

import (
	"fmt"
	"strings"
)

// PodPeakAssumptions are bounded, server-selected explanations retained with
// the demand binding. No workflow-authored text is used in admission notices.
type PodPeakAssumptions uint8

const (
	PodPeakDynamic PodPeakAssumptions = 1 << iota
	PodPeakNested
	PodPeakGraph
	PodPeakConditions
)

func (a PodPeakAssumptions) Valid() bool {
	return a & ^(PodPeakDynamic|PodPeakNested|PodPeakGraph|PodPeakConditions) == 0
}

func (d PodDemand) ConcurrencyMessage() string {
	if !d.Valid() {
		return ""
	}
	parts := []string{fmt.Sprintf("up to %d pods at once", d.Pods)}
	if d.Assumptions&PodPeakDynamic != 0 {
		parts = append(parts, "Dynamic fan-out uses the full parallelism bound because its item count is unknown or ambiguous.")
	}
	if d.Assumptions&PodPeakNested != 0 {
		parts = append(parts, "Nested template limits cap child invocations; each child retains its full inner fan-out demand.")
	}
	if d.Assumptions&PodPeakGraph != 0 {
		parts = append(parts, "Graph ordering is unavailable; each workflow slot reserves the largest declared pod in each resource dimension.")
	}
	if d.Assumptions&PodPeakConditions != 0 {
		parts = append(parts, "Conditions and early-start dependencies earn no concurrency reduction; potentially overlapping work is charged together.")
	}
	return strings.Join(parts, " ")
}
