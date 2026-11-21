package argojob_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/stateport"
)

type observationBudgetRuns struct {
	recoveredOwner
	list func(context.Context)
}

func (r observationBudgetRuns) Runs(ctx context.Context, _ stateport.RunFilter) ([]stateport.Run, error) {
	r.list(ctx)
	return nil, nil
}

func TestRecoveryObservationBudgetDefaultAndOverride(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		configured, want time.Duration
	}{
		{"default", 0, 30 * time.Second},
		{"negative", -time.Second, 30 * time.Second},
		{"explicit", 7 * time.Second, 7 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				reference, stop := context.WithTimeout(ctx, test.want)
				defer stop()
				want, _ := reference.Deadline()
				calls := 0
				rows := observationBudgetRuns{list: func(read context.Context) {
					calls++
					deadline, ok := read.Deadline()
					if !ok || !deadline.Equal(want) {
						t.Errorf("recovery deadline %v, want a %s budget", deadline, test.want)
					}
					cancel()
				}}
				err := argojob.New(argojob.Config{Runs: rows, ObservationTimeout: test.configured}).Run(ctx)
				if !errors.Is(err, context.Canceled) || calls == 0 {
					t.Fatal("recovery did not read and stop", err, calls)
				}
			})
		})
	}
}
