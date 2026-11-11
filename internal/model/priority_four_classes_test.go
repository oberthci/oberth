package model

import (
	"testing"
	"time"
)

func TestFourPriorityClasses(t *testing.T) {
	now := time.Now()
	for i, name := range []RunPriority{"low", "standard", "promotion", "high"} {
		if got := name.Rank(); got != i-1 {
			t.Errorf("%s rank=%d want %d", name, got, i-1)
		}
	}
	for _, name := range []RunPriority{"low", "standard", "promotion", "high"} {
		got, _ := (Run{Priority: name, QueuedAt: now}).EffectivePriority(now)
		if !name.Valid() || got != name {
			t.Errorf("priority %s did not remain canonical", name)
		}
	}
	for _, name := range []RunPriority{"normal", "highest"} {
		if name.Valid() {
			t.Errorf("removed priority %s accepted", name)
		}
	}
}

func TestFourPriorityAgingNeverManufacturesUrgency(t *testing.T) {
	now := time.Now()
	for _, base := range []RunPriority{"standard"} {
		run := Run{Priority: base, QueuedAt: now}
		got, _ := run.EffectivePriority(now.Add(24 * time.Hour))
		if got != "promotion" {
			t.Errorf("aged %s=%s want promotion, never high", base, got)
		}
	}
	for _, pinned := range []RunPriority{"low", "standard", "promotion"} {
		got, _ := (Run{Priority: pinned, PriorityOverride: pinned, QueuedAt: now}).EffectivePriority(now.Add(24 * time.Hour))
		if got != pinned {
			t.Errorf("explicit lowering %s changed to %s", pinned, got)
		}
	}
}
