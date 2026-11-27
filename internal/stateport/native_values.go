package stateport

import (
	"reflect"
	"time"

	"github.com/oberthci/oberth/internal/stepmodel"
)

// EqualStepFacts separates the public native facts from opaque diagnostics.
// Raw text has its own bounded evidence owner, never a progress event.
func EqualStepFacts(a, b []stepmodel.RunStep) bool {
	a = (stepmodel.RunSummary{Steps: a}).Clone().Steps
	b = (stepmodel.RunSummary{Steps: b}).Clone().Steps
	for i := range a {
		a[i].Detail = ""
	}
	for i := range b {
		b[i].Detail = ""
	}
	return reflect.DeepEqual(a, b)
}

// Node is raw native attempt evidence, separate from logical plan entries.
// A nil exit or credential observation remains unknown.
type Node struct {
	PodUID                                                      string `json:",omitempty"`
	NodeID, Path, Template, Boundary, PodName, Message, RetryOf string
	Attempt                                                     int
	Order                                                       []int // Native retry ancestry positions, outermost first.
	Status                                                      stepmodel.StepStatus
	PodRestarted                                                bool // A restart the controller recorded on the node.
	NeverRunSkip                                                bool // Native evidence of a skip created before execution.
	Exit                                                        *int
	Credential                                                  *bool
	StartedAt, FinishedAt                                       *time.Time
}

// Progress is one native observation of an unfinished run: its logical steps
// and Waiting, the reason of at most 40 characters that a run none of whose
// steps has started gives for what it waits. The store derives the run's
// queued or running status from them with stepmodel.Progress.
type Progress struct {
	Steps      []stepmodel.RunStep
	Waiting    string
	Diagnostic *stepmodel.NativeDiagnostic
}

func (p Progress) DiagnosticFor(reason string) *stepmodel.NativeDiagnostic {
	for _, step := range p.Steps {
		if step.DisplayReason == reason && step.Diagnostic != nil {
			return step.Diagnostic.Clone()
		}
	}
	if reason == p.Waiting {
		return p.Diagnostic.Clone()
	}
	return nil
}

// EvidenceStart declares the number of distinct native nodes in one observation.
// The count is a receipt binding, not a workload limit.
type EvidenceStart struct {
	RunID string
	Nodes int
}

// NodeBatch is one contiguous part of a declared sequence. An interrupted sequence
// cannot be completed; a new observation starts a new sequence.
type NodeBatch struct {
	RunID    string
	Sequence int64
	Offset   int
	Nodes    []Node
}

type EvidenceCompletion struct {
	RunID    string
	Sequence int64
}
