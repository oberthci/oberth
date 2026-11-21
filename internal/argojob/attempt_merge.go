package argojob

import (
	"cmp"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"maps"
	"slices"
	"sort"
)

func mergeAttempts(current, old stepmodel.RunStep, nodes map[string]stateport.Node) []stepmodel.StepAttempt {
	all := map[string]stepmodel.StepAttempt{}
	order := map[string]int{}
	for _, s := range []stepmodel.RunStep{old, current} {
		for _, a := range s.Attempts {
			if saved, ok := all[a.NodeID]; ok {
				a.PodName = cmp.Or(a.PodName, saved.PodName)
				a.Status = mergeStatus(saved.Status, a.Status)
			} else {
				order[a.NodeID] = len(order) + 1
			}
			if n, ok := nodes[a.NodeID]; ok {
				a.Status = n.Status
				a.PodName = cmp.Or(n.PodName, a.PodName)
				order[a.NodeID] = n.Attempt
			}
			all[a.NodeID] = a
		}
	}
	out := slices.Collect(maps.Values(all))
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].NodeID, out[j].NodeID
		if x, y := nodes[a], nodes[b]; len(x.Order) > 0 && len(y.Order) > 0 {
			if c := slices.Compare(x.Order, y.Order); c != 0 {
				return c < 0
			}
		}
		if x, y := nodes[a].StartedAt, nodes[b].StartedAt; x != nil && y != nil && !x.Equal(*y) {
			return x.Before(*y)
		}
		return cmp.Or(cmp.Compare(order[a], order[b]), cmp.Compare(a, b)) < 0
	})
	return out
}
func mergeStatus(old, next stepmodel.StepStatus) stepmodel.StepStatus {
	if terminalStep(old) || old == stepmodel.StepRunning && next == stepmodel.StepQueued || next == stepmodel.StepUnknown {
		return old
	}
	return next
}
func mergeLogs(current, old []stepmodel.LogSelector) []stepmodel.LogSelector {
	seen := map[stepmodel.LogSelector]bool{}
	out := []stepmodel.LogSelector{}
	for _, entry := range slices.Concat(old, current) {
		if !seen[entry] {
			out = append(out, entry)
			seen[entry] = true
		}
	}
	return out
}
