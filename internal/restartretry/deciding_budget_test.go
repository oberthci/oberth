package restartretry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOutageBudgetHasAnIndependentTwoMinuteBoundary(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		budget := New(start, time.Time{})
		when := start.Add(2*time.Minute - 100*time.Millisecond + offset)
		delay, ok := budget.Next(when)
		if ok != (offset < 0) {
			t.Fatal("retry crossed the fixed two-minute outage boundary", offset, delay, ok)
		}
		if ok && delay != 100*time.Millisecond {
			t.Fatal("initial retry delay changed", delay)
		}
	}
}

func TestCanceledWaitRefusesNonpositiveDelays(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, delay := range []time.Duration{-time.Second, 0} {
		if err := Wait(ctx, delay); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled immediate wait reported success", delay, err)
		}
	}
}
