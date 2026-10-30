package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestReadinessRCAAcceptsSlowDatabaseWithinThirtySeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		health := Health{Store: fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}}, Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{}}
		health.refreshReadiness(t.Context())
		checked := health.Readiness.checked
		health.ReadinessUpstreams = func(ctx context.Context) ([]model.Upstream, error) {
			time.Sleep(25 * time.Second)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return []model.Upstream{{Kind: "local"}}, nil
		}
		done := make(chan struct{})
		go func() { health.refreshReadiness(t.Context()); close(done) }()
		synctest.Wait()
		time.Sleep(20 * time.Second)
		if err := health.Ready(t.Context()); err != nil {
			t.Fatal("slow database removed ready endpoint", err)
		}
		time.Sleep(5 * time.Second)
		synctest.Wait()
		<-done
		if health.Readiness.checked.Equal(checked) || health.Readiness.retainedDeadlines != 0 {
			t.Fatal("25s successful read was timed out instead of published")
		}
		if err := health.Ready(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestReadinessRCATypedBusyRetainsGreenButHasBoundedAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		health := Health{Store: fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}}, Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{}}
		health.refreshReadiness(t.Context())
		checked := health.Readiness.checked
		health.ReadinessUpstreams = func(context.Context) ([]model.Upstream, error) {
			return nil, fmt.Errorf("database contention: %w", writerBusyReadinessError(5))
		}
		health.refreshReadiness(t.Context())
		if err := health.Ready(t.Context()); err != nil {
			t.Fatal("one busy sample removed endpoint", err)
		}
		if health.Readiness.checked != checked {
			t.Fatal("busy sample renewed successful age")
		}
		time.Sleep(readinessMaxAge + time.Second)
		health.refreshReadiness(t.Context())
		if !errors.Is(health.Ready(t.Context()), errReadinessStale) {
			t.Fatal("persistent busy remained ready forever")
		}
		var metrics bytes.Buffer
		health.Readiness.WriteMetrics(&metrics)
		if !strings.Contains(metrics.String(), "oberth_readiness_retained_busy_total 2") {
			t.Fatal("busy retention not diagnostic", metrics.String())
		}
		health.ReadyGate = func(context.Context) error { return errors.New("recovery pending") }
		if health.Ready(t.Context()) == nil {
			t.Fatal("recovery gate bypassed")
		}
	})
}
