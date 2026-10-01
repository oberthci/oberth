package service

import "testing"

func TestSignalsUseBoundedGlobalGenerations(t *testing.T) {
	t.Parallel()
	signals := NewSignals()

	runGeneration := signals.Run("run-one")
	if other := signals.Run("run-two"); other != runGeneration {
		t.Fatal("run waiters did not share one bounded generation")
	}
	signals.NotifyRun("run-two")
	select {
	case <-runGeneration:
	default:
		t.Fatal("run notification did not close the current generation")
	}
	if next := signals.Run("run-one"); next == runGeneration {
		t.Fatal("run notification did not replace the generation")
	}

	promotionGeneration := signals.Promotion("promotion-one")
	if other := signals.Promotion("promotion-two"); other != promotionGeneration {
		t.Fatal("promotion waiters did not share one bounded generation")
	}
	signals.NotifyPromotion("promotion-two")
	select {
	case <-promotionGeneration:
	default:
		t.Fatal("promotion notification did not close the current generation")
	}
	if next := signals.Promotion("promotion-one"); next == promotionGeneration {
		t.Fatal("promotion notification did not replace the generation")
	}
}

func TestZeroValueSignalsAreUsable(t *testing.T) {
	t.Parallel()
	var signals Signals
	runGeneration := signals.Run("run")
	promotionGeneration := signals.Promotion("promotion")
	signals.NotifyRun("run")
	signals.NotifyPromotion("promotion")
	select {
	case <-runGeneration:
	default:
		t.Fatal("zero-value run generation was not notified")
	}
	select {
	case <-promotionGeneration:
	default:
		t.Fatal("zero-value promotion generation was not notified")
	}
}
