package stateport

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stepmodel"
	"strings"
)

// NativePlanPath recognises native expansion delimiters at an admitted dynamic leaf.
func NativePlanPath(plan admissioncontract.Plan, path string) bool {
	for _, p := range plan.Entries {
		if p.Path == path || p.Dynamic && (p.Path == "" || strings.HasPrefix(path, p.Path+"(") || strings.HasPrefix(path, p.Path+".") || strings.HasPrefix(path, p.Path+"[") || strings.HasPrefix(path, p.Path+"/")) {
			return true
		}
	}
	return false
}

// ValidNativeSteps checks projection paths and statuses without interpreting evidence.
func ValidNativeSteps(plan admissioncontract.Plan, steps []stepmodel.RunStep) bool {
	seen := map[string]bool{}
	for _, s := range steps {
		if seen[s.Path] || s.AttemptCount != len(s.Attempts) || !s.Status.Valid() || !NativePlanPath(plan, s.Path) {
			return false
		}
		seen[s.Path] = true
	}
	return true
}

// NativeStepsExact describes whether every admitted dynamic leaf has expanded.
func NativeStepsExact(plan admissioncontract.Plan, steps []stepmodel.RunStep) bool {
	for _, p := range plan.Entries {
		if !p.Dynamic {
			continue
		}
		expanded := false
		for _, s := range steps {
			if s.Path != p.Path && NativePlanPath(admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{p}}, s.Path) || s.Path == p.Path && len(s.Attempts) > 0 {
				expanded = true
			}
		}
		if !expanded {
			return false
		}
	}
	return true
}
