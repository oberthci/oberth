package model

import "time"

// RunScheduling is an ephemeral read-only observation, never execution evidence.
// QueuePosition is accepted order among durable queued runs, not an ETA or
// claim order: publication gates may let later runs start first.
type RunScheduling struct {
	ObservedAt    time.Time             `json:"observed_at"`
	AgeSeconds    int64                 `json:"age_seconds"`
	State         string                `json:"state"`
	Message       string                `json:"message"`
	QueuePosition int64                 `json:"queue_position,omitempty"`
	Admission     *AdmissionObservation `json:"admission,omitempty"`
	Execution     *ExecutionObservation `json:"execution,omitempty"`
}

type AdmissionObservation struct {
	State    string `json:"state"`
	Position int    `json:"position,omitempty"`
	Size     string `json:"size,omitempty"`
	Weight   int    `json:"weight,omitempty"`
	Used     int    `json:"used"`
	Capacity int    `json:"capacity"`
	// Group is the repository-declared concurrency group the run was admitted
	// or is waiting under (#658). GroupHolder names the run of the same
	// repository currently holding that group; GroupAhead names the earlier run
	// of the same group queued ahead when no run holds it yet. Both are server
	// run IDs, set only while this run waits on its group.
	Group       string `json:"group,omitempty"`
	GroupHolder string `json:"group_holder,omitempty"`
	GroupAhead  string `json:"group_ahead,omitempty"`
	// DiskFloor reports the opt-in disk-space admission hold (#673). When
	// non-nil and BelowFloor is true, admission is held until disk frees up.
	DiskFloor *DiskFloorObservation `json:"disk_floor,omitempty"`
}

// DiskFloorObservation is the scheduling-visible state of the disk floor
// admission hold (#673).
type DiskFloorObservation struct {
	FloorBytes int64 `json:"floor_bytes"`
	FreeBytes  int64 `json:"free_bytes"`
	BelowFloor bool  `json:"below_floor"`
}

// ExecutionObservation contains only curated status vocabulary. Arbitrary
// controller messages, names, commands, specs and credentials never cross it.
type ExecutionObservation struct {
	State     string             `json:"state"`
	Phase     string             `json:"phase,omitempty"`
	Message   string             `json:"message"`
	Nodes     []SchedulingReason `json:"nodes,omitempty"`
	Pods      []SchedulingReason `json:"pods,omitempty"`
	Truncated bool               `json:"truncated,omitempty"`
}

type SchedulingReason struct {
	Phase   string `json:"phase"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}
