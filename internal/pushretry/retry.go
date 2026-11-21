package pushretry

import "time"

// MaxAttempts includes the initial publication attempt. Explicit retry starts
// a new budget; permission, policy and predecessor refusals never enter it.
const MaxAttempts = 5

// Delay returns the delay after a failed attempt, zero outside the retry budget.
func Delay(attempt int) time.Duration {
	if attempt < 1 || attempt >= MaxAttempts {
		return 0
	}
	return (2 * time.Minute) << uint(attempt-1)
}
