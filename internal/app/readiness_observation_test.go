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

func TestReadinessLogsExpiryWhileSamplerIsStuck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var messages []string
		health := Health{Store: fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}},
			Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{},
			Log: func(format string, args ...any) { messages = append(messages, fmt.Sprintf(format, args...)) }}
		health.refreshReadiness(t.Context())
		release, done := make(chan struct{}), make(chan struct{})
		health.ReadinessUpstreams = func(context.Context) ([]model.Upstream, error) {
			<-release // Simulate an uninterruptible syscall, beyond the probe deadline.
			return []model.Upstream{{Kind: "local"}}, nil
		}
		go func() { health.refreshReadiness(t.Context()); close(done) }()
		synctest.Wait()
		time.Sleep(readinessMaxAge + time.Second)
		for range 10 {
			if err := health.Ready(t.Context()); !errors.Is(err, errReadinessStale) {
				t.Fatalf("stuck sampler still ready: %v", err)
			}
		}
		if len(messages) != 1 || !strings.Contains(messages[0], "state=unready reason=stale sample_age=1m31s") {
			t.Fatalf("request-time expiry was not logged once: %v", messages)
		}
		var metrics bytes.Buffer
		health.Readiness.WriteMetrics(&metrics)
		if !strings.Contains(metrics.String(), "oberth_readiness_sample_age_seconds 91\n") {
			t.Fatal(metrics.String())
		}
		close(release)
		<-done
		health.ReadinessUpstreams = nil
		health.refreshReadiness(t.Context())
		if !strings.Contains(messages[len(messages)-1], "state=ready reason=ready") {
			t.Fatalf("recovery not logged: %v", messages)
		}
	})
}

func TestReadinessRetainedDeadlinesLogOncePerSuccessAndCountEveryAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var messages []string
		health := Health{Store: fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}},
			Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{},
			Log: func(format string, args ...any) { messages = append(messages, fmt.Sprintf(format, args...)) }}
		var metrics bytes.Buffer
		health.Readiness.WriteMetrics(&metrics)
		if !strings.Contains(metrics.String(), "oberth_readiness_sample_present 0\n") {
			t.Fatal(metrics.String())
		}
		health.refreshReadiness(t.Context())
		health.Store = readinessBlockingStore{}
		for range 2 {
			health.refreshReadiness(t.Context())
		}
		if len(messages) != 1 || !strings.Contains(messages[0], "event=retained_deadline sample_age=30s db_wait=30s") {
			t.Fatalf("deadline logging not bounded: %v", messages)
		}
		metrics.Reset()
		health.Readiness.WriteMetrics(&metrics)
		for _, want := range []string{"oberth_readiness_sample_present 1\n", "oberth_readiness_sample_age_seconds 60\n", "oberth_readiness_retained_deadlines_total 2\n"} {
			if !strings.Contains(metrics.String(), want) {
				t.Fatalf("missing %q: %s", want, metrics.String())
			}
		}
		health.Store = fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}}
		health.refreshReadiness(t.Context())
		health.Store = readinessBlockingStore{}
		health.refreshReadiness(t.Context())
		if len(messages) != 2 {
			t.Fatalf("new success did not reset deadline logging: %v", messages)
		}
		metrics.Reset()
		health.Readiness.WriteMetrics(&metrics)
		if !strings.Contains(metrics.String(), "oberth_readiness_retained_deadlines_total 3\n") {
			t.Fatal(metrics.String())
		}
	})
}

func TestReadinessCredentialDeadlineUsesOneUpstreamSnapshot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		database := &readinessCountingStore{fakeHealthStore: fakeHealthStore{upstreams: []model.Upstream{{Name: "dedicated", Kind: "ssh"}}}}
		health := Health{Store: database, Readiness: &ReadinessSnapshot{},
			Configured: func(context.Context, []model.Upstream) error { return nil }}
		health.refreshReadiness(t.Context())
		checked := health.Readiness.checked
		health.Configured = func(ctx context.Context, upstreams []model.Upstream) error {
			if len(upstreams) != 1 || upstreams[0].Name != "dedicated" {
				t.Fatalf("credential check lost sampled rows: %+v", upstreams)
			}
			<-ctx.Done()
			return fmt.Errorf("credential check: %w", ctx.Err())
		}
		health.refreshReadiness(t.Context())
		if database.reads.Load() != 2 { // One query per refresh, including the initial success.
			t.Fatalf("duplicate upstream enumeration: %d reads", database.reads.Load())
		}
		if !health.Readiness.checked.Equal(checked) || health.Ready(t.Context()) != nil {
			t.Fatal("credential timeout replaced or renewed the last green sample")
		}
	})
}

func TestWatchReadinessCadenceAfterFullBudgetAndCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		var calls []time.Duration
		health := Health{Store: fakeHealthStore{}, Readiness: &ReadinessSnapshot{},
			Configured: func(context.Context, []model.Upstream) error { return nil },
			ReadinessUpstreams: func(ctx context.Context) ([]model.Upstream, error) {
				calls = append(calls, time.Since(started))
				if len(calls) == 2 {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return []model.Upstream{{Kind: "local"}}, nil
			}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { health.WatchReadiness(ctx); close(done) }()
		synctest.Wait()
		time.Sleep(readinessInterval)
		synctest.Wait()
		time.Sleep(ReadinessProbeBudget)
		synctest.Wait()
		if len(calls) != 3 || calls[0] != 0 || calls[1] != 5*time.Second || calls[2] != readinessInterval+ReadinessProbeBudget {
			t.Fatalf("ticker cadence after overrun: %v", calls)
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("watcher did not exit immediately while waiting for a tick")
		}
	})
}

func TestWatchReadinessCancelDuringProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		health := Health{Store: fakeHealthStore{}, Readiness: &ReadinessSnapshot{},
			Configured: func(context.Context, []model.Upstream) error { return nil },
			ReadinessUpstreams: func(ctx context.Context) ([]model.Upstream, error) {
				calls++
				<-ctx.Done()
				return nil, ctx.Err()
			}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() { health.WatchReadiness(ctx); close(done) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("watcher waited for the probe deadline after cancellation")
		}
		if calls != 1 || !errors.Is(health.Ready(t.Context()), context.Canceled) {
			t.Fatal("canceled watcher published green or started another probe")
		}
	})
}
