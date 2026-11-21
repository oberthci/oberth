package argojob

import (
	"cmp"
	"github.com/oberthci/oberth/internal/stateport"
	"maps"
	"slices"
)

func mergeEvidence(out *Observation, previous []stateport.Node) {
	byID := map[string]stateport.Node{}
	for _, n := range out.Nodes {
		byID[n.NodeID] = n
	}
	for _, old := range previous {
		n, ok := byID[old.NodeID]
		if !ok {
			byID[old.NodeID] = old
			out.Complete = false
			continue
		}
		n = retainNode(n, old)
		byID[n.NodeID] = n
	}
	out.Nodes = slices.Collect(maps.Values(byID))
	slices.SortFunc(out.Nodes, func(a, b stateport.Node) int { return cmp.Compare(a.NodeID, b.NodeID) })
}

func retainNode(n, old stateport.Node) stateport.Node {
	n.Attempt = old.Attempt
	if len(old.Order) > 0 {
		n.Order = old.Order
	}
	n.Message, n.PodName = cmp.Or(n.Message, old.Message), cmp.Or(n.PodName, old.PodName)
	n.Exit = cmp.Or(n.Exit, old.Exit)
	n.StartedAt, n.FinishedAt = cmp.Or(n.StartedAt, old.StartedAt), cmp.Or(n.FinishedAt, old.FinishedAt)
	if old.Credential != nil && *old.Credential {
		n.Credential = old.Credential
	}
	n.PodRestarted = n.PodRestarted || old.PodRestarted
	n.Status = mergeStatus(old.Status, n.Status)
	return n
}
