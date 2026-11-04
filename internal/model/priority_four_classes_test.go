package model

import (
	"testing"
	"time"
)

func TestFourPriorityClassesAndLegacyReadNames(t *testing.T) {
	now := time.Now()
	for i, name := range []RunPriority{"low", "standard", "promotion", "high"} {
		if got := name.Rank(); got != i-1 {
			t.Errorf("%s rank=%d want %d", name, got, i-1)
		}
	}
	for old, want := range map[RunPriority]RunPriority{"normal": "standard", "highest": "high", "high": "high", "low": "low"} {
		got, _ := (Run{Priority: old, QueuedAt: now}).EffectivePriority(now)
		if got != want {
			t.Errorf("legacy %s displayed as %s, want %s", old, got, want)
		}
	}
}

func TestFourPriorityAgingNeverManufacturesUrgency(t *testing.T) {
	now := time.Now()
	for _, base := range []RunPriority{"standard", "normal"} {
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
