package model

import (
	"fmt"
	"time"
)

// Priority is assigned by the server, never by a repository's pipeline.
type RunPriority string

const (
	PriorityLow       RunPriority = "low"
	PriorityStandard  RunPriority = "standard"
	PriorityPromotion RunPriority = "promotion"
	PriorityHigh      RunPriority = "high"
	// Compatibility alias for PriorityStandard; legacy wire inputs are normalized separately.
	PriorityNormal = PriorityStandard
	// Compatibility alias for PriorityHigh; legacy wire inputs are normalized separately.
	PriorityHighest        = PriorityHigh
	PriorityStarvationWait = time.Hour
	// PriorityAgedReason explains standard work joining delivery FIFO, never urgency.
	PriorityAgedReason = "aged: standard run waited at least 60 minutes"
	// TagReleasePriorityReason identifies the automatic boost, so size limits
	// never downgrade an explicit admin bump or a promotion.
	TagReleasePriorityReason = "tag/release run: oldest waiting server-classified tag ref"
)

// Canonical accepts the two legacy input names without emitting them.
// Explicit legacy high/highest authority maps to high. Automatic historical
// protection rows are classified separately by the store, never as urgency.
func (p RunPriority) Canonical() RunPriority {
	switch p {
	case "normal":
		return PriorityStandard
	case "highest":
		return PriorityHigh
	default:
		return p
	}
}

func (p RunPriority) Valid() bool {
	switch p.Canonical() {
	case PriorityLow, PriorityStandard, PriorityPromotion, PriorityHigh:
		return true
	default:
		return false
	}
}

func (p RunPriority) Rank() int {
	switch p.Canonical() {
	case PriorityLow:
		return -1
	case PriorityPromotion:
		return 1
	case PriorityHigh:
		return 2
	default:
		return 0
	}
}

// EffectivePriority ages standard work only into promotion at its existing
// FIFO position. Neither aging nor a ref can manufacture high authority.
// Every explicit admin override pins its class, including a lowered promotion.
// Admission/end freezes the aging clock; running work is never preempted.
func (run Run) EffectivePriority(now time.Time) (RunPriority, string) {
	priority, reason := run.Priority.Canonical(), run.PriorityReason
	if priority == "" {
		priority, reason = PriorityStandard, "ordinary run"
	}
	if run.StartedAt != nil {
		now = *run.StartedAt
	} else if run.FinishedAt != nil {
		now = *run.FinishedAt
	}
	if run.PriorityOverride != "" {
		return priority, reason
	}
	if priority == PriorityStandard && !run.QueuedAt.IsZero() && now.Sub(run.QueuedAt) >= PriorityStarvationWait {
		return PriorityPromotion, PriorityAgedReason
	}
	return priority, reason
}

func (run Run) PriorityMessage(now time.Time) string {
	priority, reason := run.EffectivePriority(now)
	return fmt.Sprintf("Priority %s: %s.", priority, reason)
}
