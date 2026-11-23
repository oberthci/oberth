package installer_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"k8s.io/utils/clock"
)

func TestReadinessAttemptsReleaseStalledRequests(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := setup(t)
		p := prepare(t, f)
		timeline := clock.RealClock{}
		started, attempts := timeline.Now(), 0
		var previous context.Context
		f.engine.CheckEndpoint = func(ctx context.Context, _ installer.Profile) error {
			if previous != nil && previous.Err() == nil {
				t.Fatal("previous attempt still owns its context")
			}
			previous = ctx
			deadline, bounded := ctx.Deadline()
			if !bounded || deadline.Sub(timeline.Now()) != 10*time.Second {
				t.Fatal("readiness attempt lacks its ten-second bound")
			}
			attempts++
			if attempts < 3 {
				<-ctx.Done()
				return installer.NewProblem("connect", ctx.Err(), "")
			}
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			<-timer.C
			return nil
		}
		apply(t, f, p)
		if attempts != 3 || timeline.Since(started) != 25500*time.Millisecond || previous.Err() == nil {
			t.Fatal("stalled attempts delayed a slow successful read", attempts, timeline.Since(started))
		}
	})
}
