package model

import (
	"strings"
	"testing"
	"time"
)

// Aging raises a normal run one class, into high, at the 60-minute boundary
// and says so; high work never ages, nothing ages into highest, and admitted
// or terminal work freezes at its base class.
func TestEffectivePriorityAgesNormalIntoHighOnlyAndFreezes(t *testing.T) {
	queued := time.Now()
	for _, base := range []RunPriority{PriorityNormal, PriorityHigh} {
		t.Run(string(base), func(t *testing.T) {
			run := Run{Priority: base, PriorityReason: "base", QueuedAt: queued}
			if p, reason := run.EffectivePriority(queued.Add(time.Hour - time.Nanosecond)); p != base || reason != "base" {
				t.Fatalf("aged too soon: %s/%s", p, reason)
			}
			p, reason := run.EffectivePriority(queued.Add(time.Hour))
			if p != PriorityHigh {
				t.Fatalf("boundary priority/reason=%s/%s, want high", p, reason)
			}
			if base == PriorityNormal && (!strings.Contains(reason, "aged") || strings.Contains(reason, "highest")) {
				t.Fatalf("aged reason=%q", reason)
			}
			if base == PriorityHigh && reason != "base" {
				t.Fatalf("high work aged: %s/%s", p, reason)
			}
			if p, _ := run.EffectivePriority(queued.Add(24 * time.Hour)); p != PriorityHigh {
				t.Fatalf("waiting a day aged into %s", p)
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
	if p, _ := (Run{Priority: PriorityHighest, QueuedAt: queued}).EffectivePriority(queued.Add(2 * time.Hour)); p != PriorityHighest {
		t.Fatalf("highest work changed class: %s", p)
	}
}
