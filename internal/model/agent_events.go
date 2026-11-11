package model

import (
	"strings"
	"time"
)

// Agent events (#1514) are durable, server-generated notices for the agents
// that drive CI through MCP: a run finished, a retry hold waits, a run waits on
// a native Argo lock, a promotion or release finished, a publication failed.
// Each has a monotonically increasing ID, so delivery is at-least-once and a
// reconnecting agent resumes with events_since. The kinds are a fixed
// vocabulary; they never add a run status or label.
const (
	AgentEventRunFinished      = "run.finished"
	AgentEventRunHeld          = "run.held"
	AgentEventRunHoldAbandoned = "run.hold_abandoned"
	// AgentEventRunLockWait also reports a Workflow the Argo controller
	// postponed for its parallelism: lock SyncParallelismLock, postponed
	// true (#1517).
	AgentEventRunLockWait       = "run.lock_wait"
	AgentEventPromotionFinished = "promotion.finished"
	AgentEventReleasePublished  = "release.published"
	AgentEventPublicationFailed = "publication.failed"

	// MaximumAgentEventSummary bounds the one-line summary, and
	// MaximumAgentEventData the encoded structured payload of one event.
	MaximumAgentEventSummary = 512
	MaximumAgentEventData    = 8 << 10
)

// AgentEventKinds lists every kind in a stable order.
func AgentEventKinds() []string {
	return []string{AgentEventRunFinished, AgentEventRunHeld, AgentEventRunHoldAbandoned, AgentEventRunLockWait, AgentEventPromotionFinished, AgentEventReleasePublished, AgentEventPublicationFailed}
}

// ValidAgentEventKind reports whether kind is in the fixed vocabulary.
func ValidAgentEventKind(kind string) bool {
	for _, known := range AgentEventKinds() {
		if kind == known {
			return true
		}
	}
	return false
}

// AgentEvent is one stored event. Key deduplicates: one fact (a run's terminal
// result, one held Pod, one lock wait) is recorded once even when several
// observers or a restarted server see it again.
type AgentEvent struct {
	ID          int64          `json:"id"`
	Kind        string         `json:"kind"`
	At          time.Time      `json:"at"`
	RepoID      int64          `json:"-"`
	Repo        string         `json:"repo,omitempty"`
	RunID       string         `json:"run_id,omitempty"`
	PromotionID string         `json:"promotion_id,omitempty"`
	Ref         string         `json:"ref,omitempty"`
	SHA         string         `json:"sha,omitempty"`
	Summary     string         `json:"summary"`
	Data        map[string]any `json:"data,omitempty"`
	Key         string         `json:"-"`
}

// AgentEventFilter selects stored events. RepoIDs nil means every repository;
// an empty non-nil slice matches none (a scope with no repositories).
type AgentEventFilter struct {
	AfterID int64
	RepoIDs []int64
	Kinds   []string
	RunID   string
	Limit   int
}

// SyncParticipant is one run holding or waiting for one native Argo lock at
// one observation. Lock is the controller's key without the namespace, e.g.
// "ConfigMap/oberth-sync-limits/kvm" or "Mutex/oberth-release".
type SyncParticipant struct {
	Lock     string
	Role     string
	RunID    string
	RepoID   int64
	Repo     string
	Workflow string
}

// Sync participant roles.
const (
	SyncHolder = "holder"
	SyncWaiter = "waiter"
)

// SyncInterval is one stored holder or waiter interval. EndedAt is nil while
// the participant is still observed.
type SyncInterval struct {
	ID        int64      `json:"id"`
	Lock      string     `json:"lock"`
	Role      string     `json:"role"`
	RunID     string     `json:"run_id"`
	RepoID    int64      `json:"-"`
	Repo      string     `json:"repo,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	LastSeen  time.Time  `json:"last_seen"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// SyncIntervalFilter selects stored intervals, newest first.
type SyncIntervalFilter struct {
	Lock    string
	Since   time.Time
	RepoIDs []int64
	Limit   int
}

// SyncObservation is one Workflow's native synchronization state.
type SyncObservation struct {
	Holding []string
	Waiting []string
}

// SyncLockKey strips the namespace from a controller lock key
// "<ns>/<Kind>/<name>[/<key>]". Anything else is returned trimmed.
func SyncLockKey(key string) string {
	key = strings.TrimSpace(key)
	parts := strings.SplitN(key, "/", 2)
	if len(parts) == 2 {
		switch strings.SplitN(parts[1], "/", 2)[0] {
		case "ConfigMap", "Mutex", "Database":
			return parts[1]
		}
	}
	return key
}

// PromotionPrune is the durable prune_source request of one promotion: the
// source branch is deleted only after the promotion passed and the upstream
// target equals its result.
type PromotionPrune struct {
	PromotionID string    `json:"promotion_id"`
	Actor       string    `json:"actor"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason,omitempty"`
	BatchID     string    `json:"batch_id,omitempty"`
	RequestedAt time.Time `json:"requested_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Promotion prune states.
const (
	PromotionPrunePending = "pending"
	PromotionPrunePruned  = "pruned"
	PromotionPruneKept    = "kept"
)

// PromotionTag is the durable tag_after request of one promotion (#1524): an
// annotated tag pushed on the promotion result once the upstream target
// equals it. Attempts and NextRetryAt follow the publication retry policy.
type PromotionTag struct {
	PromotionID string     `json:"promotion_id"`
	Actor       string     `json:"actor"`
	Name        string     `json:"name"`
	Message     string     `json:"message"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason,omitempty"`
	ObjectSHA   string     `json:"object_sha,omitempty"`
	Attempts    int        `json:"attempts"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
	RequestedAt time.Time  `json:"requested_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Promotion tag states.
const (
	PromotionTagPending = "pending"
	PromotionTagPushed  = "pushed"
	PromotionTagFailed  = "failed"
)

// Retry hint classes (#1514): infrastructure failures are safe to retry
// unchanged; code failures need a change; unknown needs a reader.
const (
	RetryInfrastructure = "infrastructure"
	RetryCode           = "code"
	RetryUnknown        = "unknown"
)

// RetryHint classifies a run that did not pass. It never changes the run's
// status or reason line.
type RetryHint struct {
	Class       string            `json:"class"`
	Cause       string            `json:"cause"`
	Action      string            `json:"action"`
	Detail      string            `json:"detail,omitempty"`
	Publication *PublicationRetry `json:"publication,omitempty"`
}

// PublicationRetry is the automatic, bounded publication retry in progress.
type PublicationRetry struct {
	Attempt     int        `json:"attempt"`
	MaxAttempts int        `json:"max_attempts"`
	NextRetryAt *time.Time `json:"next_retry_at,omitempty"`
	Automatic   bool       `json:"automatic"`
}
