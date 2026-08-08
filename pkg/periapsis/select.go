package periapsis

import (
	"errors"
	"fmt"
)

// Trigger determines which trust-domain burns execute in a Job.
type Trigger string

const (
	TriggerCI      Trigger = "ci"
	TriggerRelease Trigger = "release"
)

// ParseTrigger validates a runner trigger value.
func ParseTrigger(value string) (Trigger, error) {
	trigger := Trigger(value)
	if trigger != TriggerCI && trigger != TriggerRelease {
		return "", fmt.Errorf("unknown trigger %q", value)
	}
	return trigger, nil
}

// Select returns an independent pipeline containing only burns admitted for
// the trigger. Dependencies outside the selected trust domain are treated as
// already satisfied evidence and removed.
func Select(p Pipeline, trigger Trigger) (Pipeline, error) {
	if err := Validate(p); err != nil {
		return Pipeline{}, err
	}
	if trigger != TriggerCI && trigger != TriggerRelease {
		return Pipeline{}, fmt.Errorf("unknown trigger %q", trigger)
	}
	selected := make(map[string]struct{}, len(p.Burns))
	for _, burn := range p.Burns {
		if trigger == TriggerRelease && burn.Type == Release || trigger == TriggerCI && burn.Type != Release {
			selected[burn.Name] = struct{}{}
		}
	}
	if len(selected) == 0 {
		return Pipeline{}, fmt.Errorf("pipeline has no burns for %s trigger", trigger)
	}
	result := Pipeline{Burns: make([]Burn, 0, len(selected))}
	for _, original := range p.Burns {
		if _, keep := selected[original.Name]; !keep {
			continue
		}
		burn := original.clone()
		dependencies := burn.DependsOn[:0]
		for _, dependency := range burn.DependsOn {
			if _, keep := selected[dependency]; keep {
				dependencies = append(dependencies, dependency)
			}
		}
		burn.DependsOn = dependencies
		result.Burns = append(result.Burns, burn)
	}
	return result, nil
}

// OrderedBurns returns a stable topological ordering.
func OrderedBurns(p Pipeline) ([]Burn, error) {
	if err := Validate(p); err != nil {
		return nil, err
	}
	completed := make(map[string]bool, len(p.Burns))
	ordered := make([]Burn, 0, len(p.Burns))
	for len(ordered) < len(p.Burns) {
		progressed := false
		for _, burn := range p.Burns {
			if completed[burn.Name] {
				continue
			}
			ready := true
			for _, dependency := range burn.DependsOn {
				if !completed[dependency] {
					ready = false
					break
				}
			}
			if ready {
				ordered = append(ordered, burn.clone())
				completed[burn.Name] = true
				progressed = true
			}
		}
		if !progressed {
			return nil, errors.New("pipeline dependency graph did not progress")
		}
	}
	return ordered, nil
}
