package installer

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/oberthci/oberth/internal/workflowpolicy"
)

// sourcePins is the source pin policy this operation applies: the saved pairs,
// unless the administrator clears them, plus the pairs added now. The result
// passes the server's own policy rules before anything is applied; an empty
// policy is left to the chart default, which denies every pair.
func sourcePins(c Choices, stored map[string]any) (map[string]any, error) {
	policy := workflowpolicy.SourcePolicy{}
	if !c.ClearSourcePins {
		saved, err := savedSourcePins(stored)
		if err != nil {
			return nil, described("source-pins", err, "Saved policy: "+err.Error())
		}
		policy = saved
	}
	for _, consumer := range slices.Sorted(maps.Keys(c.SourcePins)) {
		for _, producer := range c.SourcePins[consumer] {
			if !policy.Allows(consumer, producer) {
				policy[consumer] = append(policy[consumer], producer)
			}
		}
	}
	raw, err := json.Marshal(policy)
	if err == nil {
		_, err = workflowpolicy.ParseSourcePolicy(raw)
	}
	if err != nil {
		return nil, described("source-pins", err, err.Error())
	}
	if len(policy) == 0 {
		return nil, nil
	}
	var value map[string]any
	err = json.Unmarshal(raw, &value)
	return value, err
}

// savedSourcePins reads the policy recorded in installer values; none is the
// empty policy.
func savedSourcePins(stored map[string]any) (workflowpolicy.SourcePolicy, error) {
	server, _ := stored["server"].(map[string]any)
	runtime, _ := server["runtime"].(map[string]any)
	raw, err := json.Marshal(runtime["source_policy"])
	if err != nil {
		return workflowpolicy.SourcePolicy{}, err
	}
	if string(raw) == "null" {
		raw = nil
	}
	policy, err := workflowpolicy.ParseSourcePolicy(raw)
	if policy == nil {
		policy = workflowpolicy.SourcePolicy{}
	}
	return policy, err
}

// reviewSourcePins counts the reviewed pin pairs when the saved or the
// reviewed values hold any, and on an installed release names each pair the
// operation adds or removes; --detail lists every pair.
func reviewSourcePins(before, after map[string]any, installed bool) []string {
	saved, _ := savedSourcePins(before)
	reviewed, _ := savedSourcePins(after)
	old, next := saved.Pairs(), reviewed.Pairs()
	if len(old) == 0 && len(next) == 0 {
		return nil
	}
	lines := []string{fmt.Sprintf("Source pins: %d pairs (--source-pin C=P)", len(next))}
	if !installed {
		return lines
	}
	for _, pair := range next {
		if !slices.Contains(old, pair) {
			lines = append(lines, "Source pin added: "+pair)
		}
	}
	for _, pair := range old {
		if !slices.Contains(next, pair) {
			lines = append(lines, "Source pin removed: "+pair)
		}
	}
	return lines
}
