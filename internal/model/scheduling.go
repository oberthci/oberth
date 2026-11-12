package model

import (
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// RunScheduling is an ephemeral read-only observation, never execution evidence.
// QueuePosition is accepted order among durable queued runs, not an ETA or
// claim order: publication gates may let later runs start first.
type RunScheduling struct {
	// Requests is this run's peak pod requests, independent of admission budgets.
	Requests      *argoworkflow.PodRequests `json:"requests,omitempty"`
	ObservedAt    time.Time                 `json:"observed_at"`
	AgeSeconds    int64                     `json:"age_seconds"`
	State         string                    `json:"state"`
	Message       string                    `json:"message"`
	QueuePosition int64                     `json:"queue_position,omitempty"`
	Admission     *AdmissionObservation     `json:"admission,omitempty"`
	Execution     *ExecutionObservation     `json:"execution,omitempty"`
}

// AdmissionObservation is one run's place in the CPU admission gate. Used
// and Runs count granted reservations. Argo parallelism may further bound
// running Workflows, and Kubernetes places each Pod.
type AdmissionObservation struct {
	// Requests is the run's effective peak Pod request reservation.
	Requests *argoworkflow.PodRequests `json:"requests,omitempty"`

	Pressure *PressureDeferral `json:"pressure,omitempty"`
	State    string            `json:"state"`
	Position int               `json:"position,omitempty"`
	Used     int               `json:"used"`
	// Runs repeats Used. Zero for a scoped viewer, like Used.
	Runs int `json:"runs,omitempty"`
	// DiskFloor reports the opt-in disk-space admission hold (#673). When
	// non-nil and BelowFloor is true, admission is held until disk frees up.
	DiskFloor *DiskFloorObservation `json:"disk_floor,omitempty"`
	// BlockedBy names the run at the head of the admission queue that every
	// later run waits behind (#1021): the first claimed run the disk floor or
	// host pressure holds back. HeadOfQueue marks the run that is itself
	// that run; BlockedBy then describes it. Nil when nothing blocks the
	// queue.
	BlockedBy   *AdmissionBlocker `json:"blocked_by,omitempty"`
	HeadOfQueue bool              `json:"head_of_queue,omitempty"`
}

// AdmissionBlocker is the claimed run at the head of the admission queue that
// cannot be granted yet, which holds back every run claimed after it.
type AdmissionBlocker struct {
	Pressure *PressureDeferral `json:"pressure,omitempty"`
	RunID    string            `json:"run_id"`
	RepoID   int64             `json:"-"`
	Repo     string            `json:"repo"`
	Ref      string            `json:"ref"`
	// DiskFloor names why the head waits: the data disk is below its floor.
	DiskFloor   bool   `json:"disk_floor,omitempty"`
	CPUCapacity bool   `json:"cpu_capacity,omitempty"`
	CPUReason   string `json:"cpu_reason,omitempty"`
	// Used is how many runs hold a grant. Zero for a scoped viewer.
	Used         int       `json:"used"`
	WaitingSince time.Time `json:"waiting_since"`
}

// Reason is the parenthesized why of a blocked admission, e.g.
// "waiting for a run slot".
func (blocker AdmissionBlocker) Reason() string {
	switch {
	case blocker.DiskFloor:
		return "waiting for disk space"
	case blocker.CPUCapacity:
		if blocker.CPUReason != "" {
			return blocker.CPUReason
		}
		return "waiting for CPU capacity for the whole run"
	case blocker.Pressure != nil:
		switch blocker.Pressure.Resource {
		case "io":
			return "host IO pressure"
		case "cpu":
			return "host CPU pressure"
		case "cpu+io":
			return "host CPU and IO pressure"
		default:
			return "waiting for host pressure to settle"
		}
	}
	return "waiting for a run slot"
}

// DiskFloorObservation is the scheduling-visible state of the disk floor
// admission hold (#673).
type DiskFloorObservation struct {
	FloorBytes int64 `json:"floor_bytes"`
	FreeBytes  int64 `json:"free_bytes"`
	BelowFloor bool  `json:"below_floor"`
}

// ExecutionDeadlineMessage is a fixed presentation label, never raw controller text.
const ExecutionDeadlineMessage = "Execution deadline exceeded."

// LockWaitMessagePrefix starts the fixed-form message of a workflow or step
// queued behind native Argo synchronization: "Waiting for lock
// <Kind>/<name>[/<key>] (<free>/<limit> free)." The key is the controller's
// lock name without its namespace, admitted into the message only when it
// matches the lock-key grammar.
const LockWaitMessagePrefix = "Waiting for lock "

// ParallelismWaitMessage is the fixed-form message of a Workflow the Argo
// controller postponed because its parallelism limit is reached
// (controller.parallelism or controller.namespaceParallelism). The run is
// queued by Argo, not stalled; nothing of the controller's text is copied.
const ParallelismWaitMessage = "Waiting for an Argo workflow slot (controller parallelism)."

// SyncParallelismLock is the sync_status key under which Workflows waiting
// for, and holding, an Argo controller parallelism slot are reported. It is
// not a lock a document can declare; it follows the lock-key form so the
// history and events treat it like one.
const SyncParallelismLock = "Parallelism/workflow-controller"

// ExecutionDeadline is a read-only clock projection, never terminal evidence.
// Argo anchors workflow deadlines to status.startedAt, not Oberth admission.
type ExecutionDeadline struct {
	BudgetSeconds    int64      `json:"budget_seconds"`
	Clock            string     `json:"clock"`
	CreatedAt        *time.Time `json:"created_at,omitempty"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	RemainingSeconds *int64     `json:"remaining_seconds,omitempty"`
	Elapsed          bool       `json:"elapsed"`
}

// ExecutionObservation contains only curated vocabulary and execution timestamps.
// Controller messages, names, commands, specs and credentials never cross it.
type ExecutionObservation struct {
	ObservedAt               time.Time          `json:"observed_at,omitzero"`
	FinishedAt               *time.Time         `json:"finished_at,omitempty"`
	ReconciliationLagSeconds *int64             `json:"reconciliation_lag_seconds,omitempty"`
	Deadline                 *ExecutionDeadline `json:"deadline,omitempty"`
	ServerDeadline           *ExecutionDeadline `json:"server_deadline,omitempty"`
	Reason                   string             `json:"reason,omitempty"`
	State                    string             `json:"state"`
	Phase                    string             `json:"phase,omitempty"`
	Message                  string             `json:"message"`
	Nodes                    []SchedulingReason `json:"nodes,omitempty"`
	Pods                     []SchedulingReason `json:"pods,omitempty"`
	Truncated                bool               `json:"truncated,omitempty"`
}

type SchedulingReason struct {
	Phase   string `json:"phase"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// AdmissionState is the whole in-memory admission gate as data (#1099):
// every reservation it holds. It is an ephemeral observation of one server
// process, never durable evidence. Used and Runs count runs holding a grant.
type AdmissionState struct {
	Hold         *AdmissionHold         `json:"hold,omitempty"`
	Pressure     *AdmissionPressure     `json:"pressure,omitempty"`
	Used         int                    `json:"used"`
	Runs         int                    `json:"runs"`
	Reservations []AdmissionReservation `json:"reservations"`
	Blocker      *AdmissionBlocker      `json:"blocker,omitempty"`
}

// AdmissionReservation is one claimed run's place in the gate. State is
// preparing (checkout and static reads still running), waiting (held by
// the administrator hold, the disk floor or host pressure), granted, or retained
// (granted, and kept held after its owner stopped on an error that may have
// left its Workflow running).
type AdmissionReservation struct {
	Pressure *PressureDeferral `json:"pressure,omitempty"`
	RunID    string            `json:"run_id"`
	RepoID   int64             `json:"-"`
	Repo     string            `json:"repo,omitempty"`
	Ref      string            `json:"ref,omitempty"`
	Workflow string            `json:"workflow,omitempty"`
	State    string            `json:"state"`
	Position int               `json:"position,omitempty"`
	Since    time.Time         `json:"since"`
}

// AdmissionRelease reports an operator release of one reservation (#1099).
type AdmissionRelease struct {
	RunID    string `json:"run_id"`
	State    string `json:"state"`
	Workflow string `json:"workflow,omitempty"`
	Verified bool   `json:"verified"`
	Forced   bool   `json:"forced,omitempty"`
	Warning  string `json:"warning,omitempty"`
}

// IsExecutionDeadlineReason recognizes fixed controller vocabulary, not exit signals.
func IsExecutionDeadlineReason(message string) bool {
	message = strings.TrimSpace(message)
	// The watchdog may join a Cancel error on subsequent lines, and adopted
	// Workflow observation adds this fixed wrapper. Ordinary operation timeouts
	// (for example a policy refresh) are not execution deadlines.
	first, _, _ := strings.Cut(message, "\n")
	watchdog := ExecutionDeadlineMessage + " Server watchdog expired: context deadline exceeded"
	if first == watchdog || (strings.HasPrefix(first, "observe adopted Workflow ") && strings.HasSuffix(first, ": "+watchdog)) {
		return true
	}
	switch message {
	case "DeadlineExceeded", "Deadline exceeded", "Step exceeded its deadline", "Workflow deadline exceeded", "workflow deadline exceeded", ExecutionDeadlineMessage:
		return true
	default:
		return strings.HasPrefix(message, ExecutionDeadlineMessage+" Step ") && IsResourceFailureMessage(message)
	}
}

// Pressure values are host-wide PSI percentages over ten and sixty seconds.
// CPU full is undefined at host scope and is reported as zero.
type PressureResource struct {
	SomeAvg10 float64 `json:"some_avg10"`
	FullAvg10 float64 `json:"full_avg10"`
	SomeAvg60 float64 `json:"some_avg60"`
	FullAvg60 float64 `json:"full_avg60"`
}
type PressureSample struct {
	IO     PressureResource `json:"io"`
	CPU    PressureResource `json:"cpu"`
	Memory PressureResource `json:"memory"`
}
type AdmissionPressure struct {
	CPUSomeAvg10      float64         `json:"cpu_some_avg10,omitempty"`
	IOSomeAvg10       float64         `json:"io_some_avg10,omitempty"`
	HardCPU           float64         `json:"hard_cpu,omitempty"`
	HardIO            float64         `json:"hard_io,omitempty"`
	MaxWaitSeconds    int64           `json:"max_wait_seconds,omitempty"`
	Fallback          string          `json:"fallback,omitempty"`
	Mode              string          `json:"mode"`
	ThresholdExceeded bool            `json:"threshold_exceeded"`
	Enabled           bool            `json:"enabled"`
	Available         bool            `json:"available"`
	ObservedAt        time.Time       `json:"observed_at,omitzero"`
	Sample            *PressureSample `json:"sample,omitempty"`
	IOFullAvg60Max    float64         `json:"io_full_avg60_max"`
	CPUSomeAvg60Max   float64         `json:"cpu_some_avg60_max"`
	Hysteresis        float64         `json:"hysteresis"`
	MaxDeferSeconds   int64           `json:"max_defer_seconds"`
	Holding           bool            `json:"holding"`
	Resource          string          `json:"resource,omitempty"`
	Deferred          int             `json:"deferred"`
}
type PressureDeferral struct {
	Avg10Gate       bool      `json:"avg10_gate,omitempty"`
	CPUAvg10        float64   `json:"cpu_avg10,omitempty"`
	IOAvg10         float64   `json:"io_avg10,omitempty"`
	Redacted        bool      `json:"redacted,omitempty"`
	Resource        string    `json:"resource"`
	Avg60           float64   `json:"avg60,omitempty"`
	Since           time.Time `json:"since,omitzero"`
	MaxDeferSeconds int64     `json:"max_defer_seconds,omitempty"`
}

// Public preserves an actionable cause without host-wide numeric telemetry.
func (p *PressureDeferral) Public() *PressureDeferral {
	if p == nil {
		return nil
	}
	return &PressureDeferral{Resource: p.Resource, Redacted: true}
}
func (p *PressureDeferral) Reason() string {
	if p.Redacted {
		return "deferred: host " + p.Resource + " pressure"
	}
	if p.Avg10Gate {
		return fmt.Sprintf("deferred: %s pressure gate (host PSI some avg10: cpu %.1f%%, io %.1f%%)", p.Resource, p.CPUAvg10, p.IOAvg10)
	}
	return fmt.Sprintf("deferred: %s pressure %.1f%%", p.Resource, p.Avg60)
}
