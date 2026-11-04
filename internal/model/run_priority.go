package model

import (
	"fmt"
	"time"
)

// Priority is assigned by the server, never by a repository's pipeline.
type RunPriority string

const (
	PriorityNormal         RunPriority = "normal"
	PriorityHigh           RunPriority = "high"
	PriorityHighest        RunPriority = "highest"
	PriorityStarvationWait             = time.Hour
	// PriorityAgedReason explains a normal run raised into high by waiting.
	PriorityAgedReason = "aged: normal run waited at least 60 minutes"
	// TagReleasePriorityReason identifies the automatic boost, so size limits
	// never downgrade an explicit admin bump or a promotion.
	TagReleasePriorityReason = "tag/release run: oldest waiting server-classified tag ref"
)

func (p RunPriority) Rank() int {
	switch p {
	case PriorityHighest:
		return 2
	case PriorityHigh:
		return 1
	default:
		return 0
	}
}

// EffectivePriority ages a normal run that has waited PriorityStarvationWait
// up one class, into high, at its own queue position. It never ages work
// into highest: promotions, the oldest waiting tag release and explicit
// administrator bumps keep that class to themselves, so a backed-up queue
// cannot turn every run "highest" and erase the priority line. Within a
// class the order stays FIFO, which bounds the aged run's wait: every high
// run accepted after it waits behind it. Admission/end freezes aging.
func (run Run) EffectivePriority(now time.Time) (RunPriority, string) {
	priority, reason := run.Priority, run.PriorityReason
	if priority == "" {
		priority, reason = PriorityNormal, "ordinary run"
	}
	if run.StartedAt != nil {
		now = *run.StartedAt
	} else if run.FinishedAt != nil {
		now = *run.FinishedAt
	}
	if priority == PriorityNormal && !run.QueuedAt.IsZero() && now.Sub(run.QueuedAt) >= PriorityStarvationWait {
		return PriorityHigh, PriorityAgedReason
	}
	return priority, reason
}

func (run Run) PriorityMessage(now time.Time) string {
	priority, reason := run.EffectivePriority(now)
	return fmt.Sprintf("Priority %s: %s.", priority, reason)
}
