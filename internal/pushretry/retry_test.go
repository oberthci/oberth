package pushretry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTransientPushClassification(t *testing.T) {
	for _, tc := range []struct {
		message   string
		transient bool
	}{
		{"remote: Forgejo: Internal Server Connection Error\n! [remote rejected] sha -> main (pre-receive hook declined)", true},
		{"remote: Internal Server Error\npre-receive hook declined", true},
		{"fatal: unable to access host: The requested URL returned error: 503", true},
		{"connection reset by peer", true}, {"i/o timeout", true}, {"Connection timed out", true},
		{"pre-receive hook declined", false}, {"non-fast-forward", false},
		{"remote: protected " + "branch; pre-receive hook declined", false},
		{"Permission denied (publickey)", false}, {"authentication failed", false},
		{"remote: Internal Server Error\nremote: permission denied", false},
		{"remote: Internal Server Error\nnon-fast-forward", false},
	} {
		t.Run(tc.message, func(t *testing.T) {
			if got := Transient(errors.New(tc.message)); got != tc.transient {
				t.Fatalf("Transient = %v, want %v", got, tc.transient)
			}
		})
	}
	if Transient(errors.Join(errors.New("pre-receive hook declined"), context.DeadlineExceeded)) {
		t.Fatal("a verification timeout hid a permanent hook refusal")
	}
	if !Transient(context.DeadlineExceeded) || Transient(context.Canceled) || Transient(nil) {
		t.Fatal("context error classification")
	}
}

func TestRetryBudget(t *testing.T) {
	var total time.Duration
	for attempt, want := range []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 0} {
		if got := Delay(attempt + 1); got != want {
			t.Fatalf("delay after %d = %v, want %v", attempt+1, got, want)
		}
		total += want
	}
	if MaxAttempts != 5 || total != 30*time.Minute || Delay(0) != 0 || Delay(6) != 0 {
		t.Fatal("retry must be bounded to five attempts over thirty minutes")
	}
}
