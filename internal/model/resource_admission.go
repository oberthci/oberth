package model

// ResourceAdmissionFallback contains server-selected reason codes only.
type ResourceAdmissionFallback string

const (
	ResourceFallbackBudget ResourceAdmissionFallback = "budget"
	ResourceFallbackGraph  ResourceAdmissionFallback = "graph"
	ResourceFallbackVM     ResourceAdmissionFallback = "vm_undeclared"
)

func (r ResourceAdmissionFallback) Valid() bool {
	return r == "" || r == ResourceFallbackBudget || r == ResourceFallbackGraph || r == ResourceFallbackVM
}

func (r ResourceAdmissionFallback) Message() string {
	switch r {
	case ResourceFallbackBudget:
		return "Using legacy size admission: declared demand including helper reserve exceeds the numeric budget."
	case ResourceFallbackGraph:
		return "Using legacy size admission: the declared resource graph cannot be bounded."
	case ResourceFallbackVM:
		return "Using legacy size admission: VM CPU or memory declarations are unavailable."
	default:
		return ""
	}
}
