// Package testtimeout provides wall-clock budgets for test fixtures only.
// Production packages must not import it. Fake clocks and product timeout
// contracts should keep their literal durations.
package testtimeout

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Parse at test-binary startup so invalid CI configuration always fails.
var scale = func() time.Duration {
	raw, set := os.LookupEnv("OBERTH_TEST_TIMEOUT_SCALE")
	if !set {
		return 1
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > 100 {
		panic(fmt.Sprintf("OBERTH_TEST_TIMEOUT_SCALE must be an integer in 1..100, got %q", raw))
	}
	return time.Duration(value)
}()

// Scale adjusts deliberately elapsed fixture timers, observation windows and
// polling intervals. It adds no minimum delay.
func Scale(base time.Duration) time.Duration { return base * scale }

// Start bounds expected asynchronous progress (startup, acquisition, completion
// or shutdown). The 30-second floor lets a loaded host schedule the worker;
// successful operations return immediately without waiting out this budget.
func Start(base time.Duration) time.Duration { return max(Scale(base), 30*time.Second) }
