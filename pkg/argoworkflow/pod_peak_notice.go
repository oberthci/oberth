package argoworkflow

// PodPeakAssumptions are bounded, server-selected explanations retained with
// the demand binding. No workflow-authored text is used in admission notices.
type PodPeakAssumptions uint8

const (
	PodPeakDynamic PodPeakAssumptions = 1 << iota
	PodPeakNested
	PodPeakGraph
	PodPeakConditions
)

func (a PodPeakAssumptions) Valid() bool {
	return a & ^(PodPeakDynamic|PodPeakNested|PodPeakGraph|PodPeakConditions) == 0
}
