package stateport

import (
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// ProjectSteps folds native attempts into plan-ordered logical steps. Structural
// nodes outside the plan stay hidden; dynamic children inherit their plan facts.
func ProjectSteps(run Run, nodes []Node) []stepmodel.RunStep {
	entries := make(map[string]stepmodel.RunStep)
	plan := run.Admission.Plan
	for _, entry := range plan.Entries {
		status := stepmodel.StepPending
		if entry.Dynamic && (run.Status == stepmodel.RunPassed || run.Status == stepmodel.RunFailed || run.Status == stepmodel.RunCancelled) {
			status = stepmodel.StepUnknown
		}
		entries[entry.Path] = stepmodel.RunStep{Path: entry.Path, Name: entry.Name, CredentialIntent: entry.CredentialIntent, Status: status}
	}
	nodes = append([]Node(nil), nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Attempt != nodes[j].Attempt {
			return nodes[i].Attempt < nodes[j].Attempt
		}
		return nodes[i].NodeID < nodes[j].NodeID
	})
	for _, node := range nodes {
		step := entries[node.Path]
		if step.Path == "" {
			if parent, ok := dynamicParent(plan, node.Path); ok {
				step.Name = parent.Name + " [" + strings.TrimPrefix(node.Path, parent.Path+"/") + "]"
				step.CredentialIntent = parent.CredentialIntent
			}
		}
		step.Path, step.Status, step.Detail = node.Path, node.LogicalStatus(), node.Message
		step.StartedAt, step.FinishedAt = node.StartedAt, node.FinishedAt
		step.Attempts = append(step.Attempts, stepmodel.StepAttempt{NodeID: node.NodeID, PodName: node.PodName, Status: node.Status})
		step.AttemptCount = len(step.Attempts)
		entries[node.Path] = step
	}
	return orderedSteps(plan, entries)
}

// PlanSteps preserves observed facts in plan order with the plan's credential intent.
// Dynamic children remain at their parent's position, ordered by their item path.
func PlanSteps(plan admissioncontract.Plan, steps []stepmodel.RunStep) []stepmodel.RunStep {
	entries := make(map[string]stepmodel.RunStep, len(steps))
	for _, step := range steps {
		entries[step.Path] = step
	}
	out := orderedSteps(plan, entries)
	for _, step := range out {
		delete(entries, step.Path)
	}
	for _, path := range slices.Sorted(maps.Keys(entries)) {
		out = append(out, entries[path].Clone())
	}
	return out
}

func dynamicParent(plan admissioncontract.Plan, path string) (admissioncontract.PlanEntry, bool) {
	var parent admissioncontract.PlanEntry
	found := false
	for _, entry := range plan.Entries {
		if entry.Dynamic && path != entry.Path && NativePlanPath(admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{entry}}, path) && (!found || len(entry.Path) > len(parent.Path)) {
			parent = entry
			found = true
		}
	}
	return parent, found
}

func orderedSteps(plan admissioncontract.Plan, entries map[string]stepmodel.RunStep) []stepmodel.RunStep {
	children := make(map[string][]stepmodel.RunStep)
	for path, step := range entries {
		if parent, ok := dynamicParent(plan, path); ok {
			children[parent.Path] = append(children[parent.Path], step)
		}
	}
	result := make([]stepmodel.RunStep, 0, len(entries))
	for _, entry := range plan.Entries {
		if step, ok := entries[entry.Path]; ok {
			step.CredentialIntent = entry.CredentialIntent
			result = append(result, step.Clone())
		}
		group := children[entry.Path]
		sort.Slice(group, func(i, j int) bool { return expandedStepLess(entry.Path, group[i].Path, group[j].Path) })
		for _, child := range group {
			child.CredentialIntent = entry.CredentialIntent
			result = append(result, child.Clone())
		}
	}
	return result
}

// LogicalStatus retains uncertainty when a native success has no exit evidence.
func (n Node) LogicalStatus() stepmodel.StepStatus {
	if n.Status == stepmodel.StepPassed && n.Exit == nil {
		return stepmodel.StepUnknown
	}
	return n.Status
}
