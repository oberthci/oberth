package runtimeconfig

import "slices"

func (s WorkflowDefaultsSnapshot) Clone() WorkflowDefaultsSnapshot {
	s.WorkflowDefaults = slices.Clone(s.WorkflowDefaults)
	return s
}
