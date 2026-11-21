package argojob

import (
	"testing"
	"time"
)

func (c *holdTestClock) duration(timer <-chan time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.durations[timer]
}
func TestBRequestBudgetDurations(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		remaining, spent time.Duration
		request          int64
		want             time.Duration
	}{
		{"short_request", 20 * time.Minute, 0, 1, time.Millisecond},
		{"short_hold", 12 * time.Millisecond, 0, 1000, 12 * time.Millisecond},
		{"default_request", 20 * time.Minute, 0, 0, time.Minute},
		{"default_short_hold", 12 * time.Millisecond, 0, 0, 12 * time.Millisecond},
		{"time_spent_starting", 2 * time.Second, 1500 * time.Millisecond, 1000, 500 * time.Millisecond},
		{"start_exhausts_hold", time.Second, 2 * time.Second, 500, -time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			o, _, clock := testHoldOwner(t)
			o.deadline = clock.Now().Add(test.remaining)
			start := o.runtime.start
			o.runtime.start = func(argv []string) (*inspection, error) {
				child, err := start(argv)
				clock.advance(test.spent)
				return child, err
			}
			child, frame, timer, _ := o.admit(holdRequest{Version: 1, UID: "uid", Op: "exec", TimeoutMS: test.request, Argv: []string{"sh", "-c", "kill -STOP $$"}})
			if child == nil {
				t.Fatal("positive admission refused", frame)
			}
			cleanupInspection(t, child)
			defer child.out.Close()
			defer child.detail.Close()
			if got := clock.duration(timer); got != test.want {
				t.Errorf("owner request budget = %s, want %s", got, test.want)
			}
			o.close()
			<-child.done
		})
	}
}
