package model

// ResourceAdmissionFallback contains server-selected reason codes only.
type ResourceAdmissionFallback string

const (
	ResourceFallbackBudget   ResourceAdmissionFallback = "budget"
	ResourceFallbackGraph    ResourceAdmissionFallback = "graph"
	ResourceFallbackVM       ResourceAdmissionFallback = "vm_undeclared"
	ResourceFallbackMemory   ResourceAdmissionFallback = "memory_unbounded"
	ResourceFallbackRequests ResourceAdmissionFallback = "requests_undeclared"
)

func (r ResourceAdmissionFallback) Valid() bool {
	return r == "" || r == ResourceFallbackBudget || r == ResourceFallbackGraph || r == ResourceFallbackVM || r == ResourceFallbackMemory || r == ResourceFallbackRequests
}

func (r ResourceAdmissionFallback) Message() string {
	switch r {
	case ResourceFallbackBudget:
		return "Using legacy size admission: declared demand including helper reserve exceeds the numeric budget."
	case ResourceFallbackGraph:
		return "Using legacy size admission: the declared resource graph cannot be bounded."
	case ResourceFallbackVM:
		return "Using legacy size admission: VM CPU or memory declarations are unavailable."
	case ResourceFallbackMemory:
		return "Using legacy size admission: a pod or VM container lacks a positive declared memory limit."
	case ResourceFallbackRequests:
		return "Using legacy size admission: a pod container lacks positive declared CPU/memory requests."
	default:
		return ""
	}
}
