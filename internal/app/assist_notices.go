package app

import (
	"fmt"
	"github.com/oberthci/oberth/internal/admissioncontract"
)

type assistRefusal struct{ name, reason string }

func assistRefusals(enabled bool, plan admissioncontract.Plan) []assistRefusal {
	var refusals []assistRefusal
	if enabled {
		for _, e := range plan.Entries {
			if !e.HoldEligible && e.HoldRefusal != "" {
				refusals = append(refusals, assistRefusal{e.Name, e.HoldRefusal})
			}
		}
	}
	return refusals
}
func assistRefusalLines(refusals []assistRefusal) []string {
	var lines []string
	seen := map[string]bool{}
	for _, r := range refusals {
		if seen[r.reason] {
			continue
		}
		seen[r.reason] = true
		if len(lines) == 4 {
			continue
		}
		line := "assist not used: " + r.reason
		named := "assist not used: " + r.name + ": " + r.reason
		if r.name != "" && len(named) <= 80 {
			line = named
		}
		lines = append(lines, line)
	}
	if more := len(seen) - len(lines); more > 0 {
		lines = append(lines, fmt.Sprintf("assist not used: and %d more reasons", more))
	}
	return lines
}
