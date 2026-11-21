package fake

import (
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"slices"
)

func mergeNativeNodes(previous, batch []stateport.Node) ([]stateport.Node, error) {
	out := cloneNodes(previous)
	indices := map[string]int{}
	for i, n := range out {
		indices[n.NodeID] = i
	}
	for _, n := range cloneNodes(batch) {
		i, ok := indices[n.NodeID]
		if !ok {
			out = append(out, n)
			continue
		}
		if !validNativeUpdate(out[i], n) {
			return nil, runProblem(publicerr.Conflict)
		}
		out[i] = n
	}
	return out, nil
}
func validNativeUpdate(old, n stateport.Node) bool {
	if !sameNativeIdentity(old, n) {
		return false
	}
	if old.Credential != nil && *old.Credential && (n.Credential == nil || !*n.Credential) {
		return false
	}
	if (old.Status == stepmodel.StepFailed || old.Status == stepmodel.StepPassed || old.Status == stepmodel.StepSkipped) && old.Status != n.Status {
		return false
	}
	return retainsNativeEvidence(old, n)
}
func retainsNativeEvidence(old, n stateport.Node) bool {
	return (!old.PodRestarted || n.PodRestarted) && retainsNative(old.Exit, n.Exit) && retainsNative(old.StartedAt, n.StartedAt) && retainsNative(old.FinishedAt, n.FinishedAt)
}
func retainsNative[T comparable](old, next *T) bool {
	return old == nil || next != nil && *old == *next
}

func sameNativeIdentity(old, n stateport.Node) bool {
	return (len(old.Order) == 0 || slices.Equal(old.Order, n.Order)) && old.Path == n.Path && old.Attempt == n.Attempt && old.Template == n.Template && old.Boundary == n.Boundary && old.RetryOf == n.RetryOf && (old.PodName == "" || old.PodName == n.PodName)
}
