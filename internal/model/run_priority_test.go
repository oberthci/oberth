package model

import (
	"strings"
	"testing"
	"time"
)

func TestEffectivePriorityAgesHighAndNormalAtBoundaryAndFreezes(t *testing.T) {
	queued := time.Now()
	for _, base := range []RunPriority{PriorityNormal, PriorityHigh} {
		t.Run(string(base), func(t *testing.T) {
			run := Run{Priority: base, PriorityReason: "base", QueuedAt: queued}
			if p, _ := run.EffectivePriority(queued.Add(time.Hour - time.Nanosecond)); p != base {
				t.Fatalf("aged too soon: %s", p)
			}
			if p, reason := run.EffectivePriority(queued.Add(time.Hour)); p != PriorityHighest || !strings.Contains(reason, string(base)) {
				t.Fatalf("boundary priority/reason=%s/%s", p, reason)
			}
			started := queued.Add(time.Minute)
			run.StartedAt = &started
			if p, _ := run.EffectivePriority(queued.Add(2 * time.Hour)); p != base {
				t.Fatalf("admitted work aged: %s", p)
			}
			run.StartedAt, run.FinishedAt = nil, &started
			if p, _ := run.EffectivePriority(queued.Add(2 * time.Hour)); p != base {
				t.Fatalf("terminal work aged: %s", p)
			}
		})
	}
}
