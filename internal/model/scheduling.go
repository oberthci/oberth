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

// AdmissionObservation is one run's place in the admission gate. Admission
// counts runs: Used and Runs are how many runs hold a grant, Capacity and
// MaxRuns how many may (zero: no ceiling). Kubernetes places each Pod.
type AdmissionObservation struct {
	// Requests is this run's peak pod requests, shown for context only.
	Requests *argoworkflow.PodRequests `json:"requests,omitempty"`

	Pressure *PressureDeferral `json:"pressure,omitempty"`
	State    string            `json:"state"`
	Position int               `json:"position,omitempty"`
	Used     int               `json:"used"`
	Capacity int               `json:"capacity"`
	// Runs and MaxRuns repeat the run ceiling for older clients. Zero for a
	// scoped viewer, like Used and Capacity.
	Runs    int `json:"runs,omitempty"`
	MaxRuns int `json:"max_runs,omitempty"`
	// Group is the repository-declared concurrency group the run was admitted
	// or is waiting under (#658). GroupHolder retains the first holder for older clients;
	// GroupHolders names all holders across repositories. GroupAhead names an earlier run
	// of the same group queued ahead when no run holds it yet. Both are server
	// run IDs, set only while this run waits on its group.
	Group         string           `json:"group,omitempty"`
	GroupHolder   string           `json:"group_holder,omitempty"`
	GroupLimit    int              `json:"group_limit,omitempty"`
	GroupHolders  []string         `json:"group_holders,omitempty"`
	GroupBlocked  bool             `json:"group_blocked,omitempty"`
	GroupRunRepos map[string]int64 `json:"-"`
	GroupAhead    string           `json:"group_ahead,omitempty"`
	// DiskFloor reports the opt-in disk-space admission hold (#673). When
	// non-nil and BelowFloor is true, admission is held until disk frees up.
	DiskFloor *DiskFloorObservation `json:"disk_floor,omitempty"`
	// BlockedBy names the run at the head of the admission queue that every
	// later run waits behind (#1021): the first claimed run without a run
	// slot. HeadOfQueue marks the run that is itself that run; BlockedBy then
	// describes it. Nil when the run is parked on its group or nothing
	// blocks the queue.
	BlockedBy   *AdmissionBlocker `json:"blocked_by,omitempty"`
	HeadOfQueue bool              `json:"head_of_queue,omitempty"`
}

// AdmissionBlocker is the claimed run at the head of the admission queue that
// has no run slot, which holds back every run claimed after it.
type AdmissionBlocker struct {
	Pressure *PressureDeferral `json:"pressure,omitempty"`
	RunID    string            `json:"run_id"`
	RepoID   int64             `json:"-"`
	Repo     string            `json:"repo"`
	Ref      string            `json:"ref"`
	// RunCeiling and DiskFloor name why the head waits: every run slot is
	// taken, or the data disk is below its floor.
	RunCeiling bool `json:"run_ceiling,omitempty"`
	DiskFloor  bool `json:"disk_floor,omitempty"`
	// Used and Capacity are the run ceiling: runs holding a grant and the
	// configured maximum. Zero for a scoped viewer.
	Used         int       `json:"used"`
	Capacity     int       `json:"capacity"`
	WaitingSince time.Time `json:"waiting_since"`
}

// Reason is the parenthesized why of a blocked admission, e.g.
// "waiting for a run slot".
func (blocker AdmissionBlocker) Reason() string {
	switch {
	case blocker.DiskFloor:
		return "waiting for disk space"
	case blocker.RunCeiling:
		switch blocker.Used {
		case 0:
			return "waiting for a run slot"
		case 1:
			return "1 run running"
		}
		return fmt.Sprintf("%d runs running", blocker.Used)
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

// AdmissionCapacityConfig describes the effective operator capacity and reload health.
type AdmissionCapacityConfig struct {
	MaxConcurrentJobs int       `json:"max_concurrent_jobs"`
	BootDefault       int       `json:"boot_default"`
	Source            string    `json:"source"`
	File              string    `json:"file,omitempty"`
	Error             string    `json:"error,omitempty"`
	ChangedAt         time.Time `json:"changed_at,omitzero"`
}

// AdmissionState is the whole in-memory admission gate as data (#1099): its
// run ceiling, every reservation it holds, and every concurrency group in
// use. It is an ephemeral observation of one server process, never durable
// evidence. Used and Runs count runs holding a grant; Capacity and MaxRuns
// are the ceiling, zero when none is configured.
type AdmissionState struct {
	Hold          *AdmissionHold           `json:"hold,omitempty"`
	Configuration *AdmissionCapacityConfig `json:"configuration,omitempty"`
	Pressure      *AdmissionPressure       `json:"pressure,omitempty"`
	Capacity      int                      `json:"capacity"`
	Used          int                      `json:"used"`
	Runs          int                      `json:"runs"`
	MaxRuns       int                      `json:"max_runs"`
	Reservations  []AdmissionReservation   `json:"reservations"`
	Groups        []AdmissionGroup         `json:"groups"`
	Blocker       *AdmissionBlocker        `json:"blocker,omitempty"`
}

// AdmissionReservation is one claimed run's place in the gate. State is
// preparing (checkout and static reads still running), waiting (for a run
// slot), group (parked on its concurrency group), granted, or retained
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
	Group    string            `json:"group,omitempty"`
	Position int               `json:"position,omitempty"`
	Since    time.Time         `json:"since"`
}

// AdmissionGroup is a YAML-defined shared group with current holders and waiters.
type AdmissionGroup struct {
	Name    string   `json:"name"`
	Limit   int      `json:"limit"`
	Holders []string `json:"holders"`
	RepoID  int64    `json:"-"`
	Repo    string   `json:"repo,omitempty"`
	Holder  string   `json:"holder_run_id,omitempty"`
	Waiting []string `json:"waiting,omitempty"`
}

// AdmissionRelease reports an operator release of one reservation (#1099).
type AdmissionRelease struct {
	RunID    string `json:"run_id"`
	State    string `json:"state"`
	Group    string `json:"group,omitempty"`
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
