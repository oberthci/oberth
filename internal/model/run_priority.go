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

// EffectivePriority ages normal and high work into the highest class, where
// original FIFO position precedes newer releases. Admission/end freezes aging.
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
	if (priority == PriorityNormal || priority == PriorityHigh) && !run.QueuedAt.IsZero() && now.Sub(run.QueuedAt) >= PriorityStarvationWait {
		return PriorityHighest, fmt.Sprintf("starvation guard: %s run waited at least 60 minutes", priority)
	}
	return priority, reason
}

func (run Run) PriorityMessage(now time.Time) string {
	priority, reason := run.EffectivePriority(now)
	return fmt.Sprintf("Priority %s: %s.", priority, reason)
}
