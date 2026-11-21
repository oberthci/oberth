package stateport

import (
	"time"

	"github.com/oberthci/oberth/internal/stepmodel"
)

// Node is raw native attempt evidence, separate from logical plan entries.
// A nil exit or credential observation remains unknown.
type Node struct {
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
