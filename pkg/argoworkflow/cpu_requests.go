package argoworkflow

import "maps"

// CPURequestPolicy is a server decision, never repository authority.
type CPURequestPolicy struct {
	MaxMilli  int64            `json:"max_milli"`
	Templates map[string]int64 `json:"templates,omitempty"`
}

func (p CPURequestPolicy) Clone() CPURequestPolicy { p.Templates = maps.Clone(p.Templates); return p }
func (p CPURequestPolicy) Valid() bool {
	if p.MaxMilli < 0 || p.MaxMilli > 1_000_000_000 || len(p.Templates) > 256 {
		return false
	}
	for name, n := range p.Templates {
		if name == "" || len(name) > 253 || n < 1 || n > 1_000_000_000 {
			return false
		}
	}
	return true
}
func (r *DemandReceipt) WithCPURequestPolicy(p CPURequestPolicy) *DemandReceipt {
	copy := *r
	p = p.Clone()
	copy.cpuPolicy = &p
	return &copy
}
func (r *DemandReceipt) CPURequestPolicy() *CPURequestPolicy {
	if r == nil || r.cpuPolicy == nil {
		return nil
	}
	p := r.cpuPolicy.Clone()
	return &p
}
