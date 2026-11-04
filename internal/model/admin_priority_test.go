package model

import (
	"testing"
	"time"
)

func TestAdminPriorityAgingCannotUndoLowering(t *testing.T) {
	now := time.Now()
	for _, priority := range []RunPriority{"low", "normal"} {
		run := Run{Priority: priority, PriorityOverride: priority, PriorityReason: "admin priority: wait", QueuedAt: now.Add(-24 * time.Hour)}
		if got, why := run.EffectivePriority(now); got != priority || why != run.PriorityReason {
			t.Fatalf("override aged: %s %s", got, why)
		}
	}
	if RunPriority("low").Rank() >= PriorityNormal.Rank() {
		t.Fatal("low must sort after normal")
	}
}
