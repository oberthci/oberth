package model

import (
	"strings"
	"time"
)

// RunScheduling is an ephemeral read-only observation, never execution evidence.
// QueuePosition is accepted order among durable queued runs, not an ETA or
// claim order: publication gates may let later runs start first.
type RunScheduling struct {
	// Requests is this run's peak pod requests, independent of admission budgets.
	ObservedAt    time.Time             `json:"observed_at"`
	AgeSeconds    int64                 `json:"age_seconds"`
	State         string                `json:"state"`
	Message       string                `json:"message"`
	QueuePosition int64                 `json:"queue_position,omitempty"`
	Execution     *ExecutionObservation `json:"execution,omitempty"`
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
