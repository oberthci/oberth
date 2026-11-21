package stateport

import (
	"cmp"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stepmodel"
	"slices"
)

// ArmedStepMayHaveHeld identifies armed results that may have held.
// A Pod restart recorded on the native node excludes an armed result; a
// positively identified never-run skip remains ordinary evidence.
// A Pod replaced before the controller recorded the step as running can leave no
// native trace; closing this gap is required before assist is enabled.
// Only the last native attempt at each armed path decides; missing results fail
// closed. Eligibility and the request option alone do not mean a step was armed.
func ArmedStepMayHaveHeld(plan admissioncontract.Plan, nodes []Node) bool {
	if !slices.ContainsFunc(plan.Entries, func(e admissioncontract.PlanEntry) bool { return e.HoldArmed }) {
		return false
	}
	latest := map[string]Node{}
	for _, n := range nodes {
		old, ok := latest[n.Path]
		if !ok || nativeAttemptCompare(old, n) < 0 {
			latest[n.Path] = n
		}
	}
	for _, entry := range plan.Entries {
		if !entry.HoldArmed {
			continue
		}
		found := false
		leaf := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{entry}}
		for path, n := range latest {
			if !NativePlanPath(leaf, path) {
				continue
			}
			found = true
			if !ordinaryArmedAttempt(n) {
				return true
			}
		}
		if !found {
			return true
		}
	}
	return false
}

func ordinaryArmedAttempt(n Node) bool {
	if n.PodRestarted {
		return false
	}
	if n.Status == stepmodel.StepPassed {
		return true
	}
	return n.Status == stepmodel.StepSkipped && n.NeverRunSkip && n.PodName == "" && n.Exit == nil
}

func nativeAttemptCompare(a, b Node) int {
	if len(a.Order) > 0 && len(b.Order) > 0 {
		if c := slices.Compare(a.Order, b.Order); c != 0 {
			return c
		}
	}
	if a.StartedAt != nil && b.StartedAt != nil && !a.StartedAt.Equal(*b.StartedAt) {
		if a.StartedAt.Before(*b.StartedAt) {
			return -1
		}
		return 1
	}
	return cmp.Or(cmp.Compare(a.Attempt, b.Attempt), cmp.Compare(a.NodeID, b.NodeID))
}
