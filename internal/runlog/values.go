package runlog

import "github.com/oberthci/oberth/internal/logcontract"

const (
	DefaultRunLimitBytes int64 = 64 << 20
	MaxScanBytes         int64 = 4 << 20
	DefaultWindowLines         = 200
	maxRecordBytes             = 64 << 10
	maxIndexBytes              = 8 << 20
)

// CaptureState describes retained-output availability, not execution status.
type CaptureState string

const (
	Waiting     CaptureState = "waiting"
	Following   CaptureState = "following"
	Copying     CaptureState = "copying"
	Captured    CaptureState = "captured"
	Interrupted CaptureState = "interrupted"
)

// ContainerLog is an immutable physical receipt with caller-verified attribution.
// Reason is a complete phrase of at most 40 characters; Detail holds full text.
// ParentNodeID is process-only proof permitting refinement of a provisional node.
type ContainerLog struct {
	NodeID       string       `json:"node_id"`
	Step         string       `json:"step"`
	PodUID       string       `json:"pod_uid"`
	PodName      string       `json:"pod_name"`
	Namespace    string       `json:"namespace"`
	Container    string       `json:"container_name"`
	Start        int64        `json:"start"`
	End          int64        `json:"end"`
	Reason       string       `json:"reason"`
	Detail       string       `json:"detail,omitempty"`
	State        CaptureState `json:"state"`
	Main         bool         `json:"main,omitempty"`
	ParentNodeID string       `json:"-"`
}

type Selector = logcontract.Selector
type ReadRequest = logcontract.ReadRequest
type Filter = logcontract.Filter
type Meta = logcontract.Meta

// Gap carries caller-verified source facts; free-form text belongs only in Detail.
type Gap struct {
	Reason GapReason
	Detail string
}
type GapReason string

const (
	PodMissing GapReason = "pod_missing"
	NotRun     GapReason = "not_run"
)

type span struct{ start, end int64 }
type index struct {
	RunID          string                  `json:"run_id"`
	Size           int64                   `json:"size"`
	Reason         string                  `json:"reason"`
	Detail         string                  `json:"detail,omitempty"`
	Containers     []ContainerLog          `json:"containers"`
	Infrastructure []InfrastructureReceipt `json:"infrastructure,omitempty"`
}
