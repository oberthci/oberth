package model

import (
	"fmt"
	"time"
)

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
	VMCapacity bool   `json:"vm_capacity,omitempty"`
	State      string `json:"state"`
	Position   int    `json:"position,omitempty"`
	Size       string `json:"size,omitempty"`
	Weight     int    `json:"weight,omitempty"`
	Used       int    `json:"used"`
	Capacity   int    `json:"capacity"`
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
	// BlockedBy names the run at the head of the admission queue that every
	// later run waits behind (#1021): the first claimed run that does not
	// fit. HeadOfQueue marks the run that is itself that run; BlockedBy then
	// describes it. Nil when the run is parked on its group or VM capacity
	// or nothing blocks the queue.
	BlockedBy   *AdmissionBlocker `json:"blocked_by,omitempty"`
	HeadOfQueue bool              `json:"head_of_queue,omitempty"`
}

// AdmissionBlocker is the claimed run at the head of the weighted admission
// queue that does not fit, which holds back every run claimed after it.
type AdmissionBlocker struct {
	RunID  string `json:"run_id"`
	RepoID int64  `json:"-"`
	Repo   string `json:"repo"`
	Ref    string `json:"ref"`
	Size   string `json:"size"`
	Weight int    `json:"weight"`
	// FullDrain is set for an XL run, which is admitted only once nothing
	// else holds weight.
	FullDrain bool `json:"full_drain,omitempty"`
	// RunCeiling and DiskFloor name the other reasons a run that would fit
	// the weight budget still waits.
	RunCeiling   bool      `json:"run_ceiling,omitempty"`
	DiskFloor    bool      `json:"disk_floor,omitempty"`
	Used         int       `json:"used"`
	Capacity     int       `json:"capacity"`
	WaitingSince time.Time `json:"waiting_since"`
}

// Reason is the parenthesized why of a blocked admission, e.g.
// "XL, needs a full drain".
func (blocker AdmissionBlocker) Reason() string {
	switch {
	case blocker.DiskFloor:
		return blocker.Size + ", held by the disk floor"
	case blocker.FullDrain:
		return blocker.Size + ", needs a full drain"
	case blocker.RunCeiling:
		return blocker.Size + ", waiting for a run slot"
	case blocker.Capacity == 0:
		// A scoped view omits the shared budget's use.
		return fmt.Sprintf("%s, needs %d weight", blocker.Size, blocker.Weight)
	default:
		return fmt.Sprintf("%s, needs %d weight; %d/%d used", blocker.Size, blocker.Weight, blocker.Used, blocker.Capacity)
	}
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
