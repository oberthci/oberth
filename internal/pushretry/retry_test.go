package pushretry

import (
	"testing"
	"time"
)

func TestFiveAttemptsAndThirtyMinuteBudget(t *testing.T) {
	var sum time.Duration
	for attempt, want := range []time.Duration{0, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 0, 0} {
		if got := Delay(attempt); got != want {
			t.Fatalf("attempt %d: %v", attempt, got)
		}
		sum += want
	}
	if MaxAttempts != 5 || sum != 30*time.Minute {
		t.Fatal("budget exceeded")
	}
}
func TestInvalidAttemptRefusesRetry(t *testing.T) {
	if Delay(-1) != 0 || Delay(int(^uint(0)>>1)) != 0 {
		t.Fatal("invalid attempt retried")
	}
}
