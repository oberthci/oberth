package model

// ResourceAdmissionFallback contains server-selected reason codes only.
type ResourceAdmissionFallback string

const (
	ResourceFallbackBudget   ResourceAdmissionFallback = "budget"
	ResourceFallbackGraph    ResourceAdmissionFallback = "graph"
	ResourceFallbackExternal ResourceAdmissionFallback = "vm_undeclared"
	ResourceFallbackMemory   ResourceAdmissionFallback = "memory_unbounded"
	ResourceFallbackRequests ResourceAdmissionFallback = "requests_undeclared"
)

func (r ResourceAdmissionFallback) Valid() bool {
	return r == "" || r == ResourceFallbackBudget || r == ResourceFallbackGraph || r == ResourceFallbackExternal || r == ResourceFallbackMemory || r == ResourceFallbackRequests
}

func (r ResourceAdmissionFallback) Message() string {
	switch r {
	case ResourceFallbackBudget:
		return "legacy size admission: commit measured requests/limits in git; declared demand including helper reserve exceeds the numeric budget."
	case ResourceFallbackGraph:
		return "legacy size admission: commit measured requests/limits in git; the declared resource graph cannot be bounded."
	case ResourceFallbackExternal:
		return "legacy size admission: commit measured requests/limits in git; historical external resource declarations are unavailable."
	case ResourceFallbackMemory:
		return "legacy size admission: commit measured requests/limits in git; a pod container lacks a positive declared memory limit."
	case ResourceFallbackRequests:
		return "legacy size admission: commit measured requests/limits in git; a pod container lacks positive declared CPU/memory requests."
	default:
		return ""
	}
}
