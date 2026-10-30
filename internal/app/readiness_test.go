package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/liveness"
	"github.com/oberthci/oberth/internal/model"
)

type readinessCountingStore struct {
	fakeHealthStore
	reads atomic.Int32
}

func (store *readinessCountingStore) ListUpstreams(ctx context.Context) ([]model.Upstream, error) {
	store.reads.Add(1)
	return store.fakeHealthStore.ListUpstreams(ctx)
}

func TestReadinessSnapshotNeverProbesOnRequestAndFailsClosed(t *testing.T) {
	database := &readinessCountingStore{fakeHealthStore: fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}}}
	health := Health{
		Store:      database,
		Configured: func(context.Context, []model.Upstream) error { return nil },
		Cluster:    func(context.Context) error { t.Fatal("readiness called Kubernetes"); return nil },
		Audit:      func(context.Context) error { t.Fatal("readiness called audit"); return nil },
		VCS:        func(context.Context, model.Upstream) error { t.Fatal("readiness called upstream"); return nil },
		Readiness:  &ReadinessSnapshot{},
	}
	if health.Ready(t.Context()) == nil {
		t.Fatal("ready without sample")
	}
	health.refreshReadiness(t.Context())
	for range 100 {
		if err := health.Ready(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if database.reads.Load() != 1 {
		t.Fatalf("readiness requests made %d database reads", database.reads.Load())
	}
	health.ReadyGate = func(context.Context) error { return errors.New("recovering") }
	if health.Ready(t.Context()) == nil {
		t.Fatal("startup gate bypassed")
	}
	health.ReadyGate = nil
	setReadinessChecked(health.Readiness, time.Now().Add(-readinessMaxAge-time.Second))
	if health.Ready(t.Context()) == nil {
		t.Fatal("stale green sample accepted")
	}
	database.err = errors.New("database unavailable")
	health.refreshReadiness(t.Context())
	if health.Ready(t.Context()) == nil {
		t.Fatal("failed refresh retained green sample")
	}
	database.err = nil
	health.refreshReadiness(t.Context())
	if err := health.Ready(t.Context()); err != nil {
		t.Fatalf("database recovered but readiness did not: %v", err)
	}
}

func setReadinessChecked(snapshot *ReadinessSnapshot, checked time.Time) {
	snapshot.mu.Lock()
	defer snapshot.mu.Unlock()
	snapshot.checked = checked
}

type readinessBlockingStore struct {
	fakeHealthStore
}

func (store readinessBlockingStore) ListUpstreams(ctx context.Context) ([]model.Upstream, error) {
	<-ctx.Done()
	return nil, fmt.Errorf("waiting for connection: %w", ctx.Err())
}

func TestReadinessDeadlineRetainsGreenWithoutRenewingAge(t *testing.T) {
	for _, test := range []struct {
		name string
		age  time.Duration
	}{
		{name: "fresh", age: time.Second},
		{name: "stale", age: readinessMaxAge + time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			health := Health{
				Store:      fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}},
				Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{},
			}
			health.refreshReadiness(t.Context())
			checked := time.Now().Add(-test.age)
			setReadinessChecked(health.Readiness, checked)
			health.Store = readinessBlockingStore{}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
			defer cancel()
			health.refreshReadiness(ctx)
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatal("store did not wait for the deadline")
			}
			health.Readiness.mu.RLock()
			retained := health.Readiness.checked.Equal(checked) && health.Readiness.err == nil
			health.Readiness.mu.RUnlock()
			if !retained {
				t.Fatal("deadline replaced or renewed the last green sample")
			}
			err := health.Ready(t.Context())
			if test.age < readinessMaxAge && err != nil {
				t.Fatalf("fresh green sample lost after deadline: %v", err)
			}
			if test.age > readinessMaxAge && !errors.Is(err, errReadinessStale) {
				t.Fatalf("expired green sample = %v, want stale", err)
			}
		})
	}
}

func TestReadinessHardErrorsImmediatelyReplaceGreen(t *testing.T) {
	for _, test := range []struct {
		name          string
		store         fakeHealthStore
		configuredErr error
		cancel        bool
		want          error
	}{
		{name: "database", store: fakeHealthStore{err: errors.New("database I/O failed")}, want: errReadinessDatabase},
		{name: "no upstream", want: errReadinessNoUpstream},
		{name: "credentials", store: fakeHealthStore{upstreams: []model.Upstream{{Kind: "ssh"}}}, configuredErr: errors.New("missing key"), want: errReadinessConfiguration},
		{name: "canceled", store: fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}}, cancel: true, want: context.Canceled},
		{name: "canceled with deadline error", store: fakeHealthStore{err: context.DeadlineExceeded}, cancel: true, want: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			health := Health{
				Store:      fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}},
				Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{},
			}
			health.refreshReadiness(t.Context())
			health.Store = test.store
			health.Configured = func(context.Context, []model.Upstream) error { return test.configuredErr }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancel {
				cancel()
			}
			health.refreshReadiness(ctx)
			if err := health.Ready(t.Context()); !errors.Is(err, test.want) {
				t.Fatalf("readiness = %v, want immediate %v", err, test.want)
			}
		})
	}
}

func TestReadinessDeadlineCannotCreateGreen(t *testing.T) {
	for _, priorErr := range []error{nil, errors.New("database unavailable")} {
		snapshot := &ReadinessSnapshot{}
		if priorErr != nil {
			snapshot.checked, snapshot.err = time.Now(), priorErr
		}
		health := Health{Store: readinessBlockingStore{}, Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: snapshot}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
		health.refreshReadiness(ctx)
		cancel()
		if err := health.Ready(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline without a prior green sample = %v, want unready", err)
		}
	}
}

func TestReadinessLogsOnlyTransitions(t *testing.T) {
	var messages []string
	database := &fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}}
	health := Health{
		Store: database, Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{},
		Log: func(format string, args ...any) { messages = append(messages, fmt.Sprintf(format, args...)) },
	}
	health.refreshReadiness(t.Context())
	health.refreshReadiness(t.Context())
	database.err = errors.New("sensitive arbitrary database detail")
	health.refreshReadiness(t.Context())
	health.refreshReadiness(t.Context())
	for range 100 {
		_ = health.Ready(t.Context())
	}
	database.err = nil
	health.refreshReadiness(t.Context())
	health.refreshReadiness(t.Context())
	if len(messages) != 2 {
		t.Fatalf("got %d logs, want two transitions: %v", len(messages), messages)
	}
	for i, expected := range []string{"state=unready reason=database_error", "state=ready reason=ready"} {
		if !strings.Contains(messages[i], expected) {
			t.Fatalf("log %d = %q, want %q", i, messages[i], expected)
		}
		for _, field := range []string{"sample_age=", "db_wait=", "probe_duration=", "outage_duration="} {
			if !strings.Contains(messages[i], field) {
				t.Fatalf("missing %q in %q", field, messages[i])
			}
		}
		if strings.Contains(messages[i], "sensitive") {
			t.Fatalf("unbounded error detail in %q", messages[i])
		}
	}
}

func TestReadinessLogsStaleDeadlineAndRecoveryGateTransitions(t *testing.T) {
	var messages []string
	health := Health{
		Store:      fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}},
		Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{},
		Log: func(format string, args ...any) { messages = append(messages, fmt.Sprintf(format, args...)) },
	}
	health.refreshReadiness(t.Context())
	health.Store = readinessBlockingStore{}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel()
	health.refreshReadiness(ctx)
	if len(messages) != 1 || !strings.Contains(messages[0], "event=retained_deadline") {
		t.Fatalf("first retained deadline was not logged: %v", messages)
	}
	setReadinessChecked(health.Readiness, time.Now().Add(-readinessMaxAge-time.Second))
	health.refreshReadiness(ctx)
	health.refreshReadiness(ctx)
	health.Store = fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}}
	health.refreshReadiness(t.Context())
	health.ReadyGate = func(context.Context) error { return errors.New("recovering") }
	health.refreshReadiness(t.Context())
	health.refreshReadiness(t.Context())
	health.ReadyGate = nil
	health.refreshReadiness(t.Context())
	want := []string{"event=retained_deadline", "state=unready reason=stale", "state=ready reason=ready", "state=unready reason=startup_recovery", "state=ready reason=ready"}
	if len(messages) != len(want) {
		t.Fatalf("logs = %v, want %v", messages, want)
	}
	for i, expected := range want {
		if !strings.Contains(messages[i], expected) {
			t.Fatalf("log %d = %q, want %q", i, messages[i], expected)
		}
	}
}

func TestReadinessDoesNotRequireDiagnosticProbes(t *testing.T) {
	health := Health{
		Store:      fakeHealthStore{upstreams: []model.Upstream{{Kind: "ssh"}}},
		Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{},
	}
	health.refreshReadiness(t.Context())
	if err := health.Ready(t.Context()); err != nil {
		t.Fatalf("optional diagnostics changed healthy serving state: %v", err)
	}
}

func TestReadinessCanceledRefreshCannotPublishGreen(t *testing.T) {
	health := Health{
		Store:      fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}},
		Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{},
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	health.refreshReadiness(ctx)
	if err := health.Ready(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh = %v, want not ready", err)
	}
}

// Replay the observed 17s age and longer recovery gaps without a load-sensitive
// wall-clock test. Slow successful reads must fit the sampler's own budget.
func TestReadinessSlowProgressUnderContention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		health := Health{
			Store:      fakeHealthStore{err: errors.New("ordinary reader pool is saturated")},
			Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{},
			ReadinessUpstreams: func(ctx context.Context) ([]model.Upstream, error) {
				select {
				case <-time.After(4 * time.Second):
					return []model.Upstream{{Kind: "local"}}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
		}
		health.refreshReadiness(t.Context())
		if err := health.Ready(t.Context()); err != nil {
			t.Fatalf("slow successful sample was refused: %v", err)
		}
		for _, gap := range []time.Duration{17 * time.Second, 49 * time.Second} {
			time.Sleep(gap)
			if err := health.Ready(t.Context()); err != nil {
				t.Fatalf("progressing sampler lost readiness after %s: %v", gap, err)
			}
			health.refreshReadiness(t.Context())
			if err := health.Ready(t.Context()); err != nil {
				t.Fatalf("slow refresh failed after %s: %v", gap, err)
			}
		}
		// A watcher that makes no more successful progress still expires.
		time.Sleep(readinessMaxAge + time.Second)
		if err := health.Ready(t.Context()); !errors.Is(err, errReadinessStale) {
			t.Fatalf("stopped sampler remained ready: %v", err)
		}
	})
}

func TestReadinessPersistentDeadlinesExpireWithoutRenewingGreen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		health := Health{Store: fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}},
			Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{}}
		health.refreshReadiness(t.Context())
		checked := health.Readiness.checked
		health.Store = readinessBlockingStore{}
		for time.Since(checked) <= readinessMaxAge {
			health.refreshReadiness(t.Context())
			if health.Readiness.checked != checked {
				t.Fatal("deadline renewed green sample")
			}
		}
		if err := health.Ready(t.Context()); !errors.Is(err, errReadinessStale) {
			t.Fatalf("persistent database timeouts remained ready: %v", err)
		}
	})
}

func TestReadinessWriterHardFailureOverridesReadableWALAndRecovers(t *testing.T) {
	database := &readinessCountingStore{fakeHealthStore: fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}}}
	var writeErr error
	health := Health{Store: database, Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{}, WriterReady: func() error { return writeErr }}
	health.refreshReadiness(t.Context())
	if err := health.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	writeErr = errors.New("disk full")
	reads := database.reads.Load()
	health.refreshReadiness(t.Context())
	if err := health.Ready(t.Context()); !errors.Is(err, errReadinessDatabase) {
		t.Fatal("readable WAL hid a hard writer failure", err)
	}
	if database.reads.Load() != reads {
		t.Fatal("writer error path unexpectedly probed storage")
	}
	writeErr = nil
	health.refreshReadiness(t.Context())
	if err := health.Ready(t.Context()); err != nil {
		t.Fatal("writer recovery did not restore readiness", err)
	}
}

type writerBusyReadinessError int

func (err writerBusyReadinessError) Error() string { return "writer contention" }
func (err writerBusyReadinessError) Code() int     { return int(err) }

func TestReadinessWriterBusyRequiresPersistentProbeErrors(t *testing.T) {
	start := time.Now()
	var elapsed atomic.Int64
	heartbeat := &liveness.Heartbeat{Now: func() time.Time { return start.Add(time.Duration(elapsed.Load())) }}
	ctx, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{})
	complete := make(chan error)
	done := make(chan struct{})
	go func() {
		defer close(done)
		heartbeat.Watch(ctx, time.Millisecond, func(ctx context.Context) error {
			select {
			case entered <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case err := <-complete:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("writer watcher did not stop")
		}
	}()
	waitProbe := func() {
		t.Helper()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("writer probe did not run")
		}
	}
	waitProbe()
	database := &readinessCountingStore{fakeHealthStore: fakeHealthStore{upstreams: []model.Upstream{{Kind: "local"}}}}
	health := Health{Store: database, Configured: func(context.Context, []model.Upstream) error { return nil }, Readiness: &ReadinessSnapshot{}, WriterReady: heartbeat.ProbeError}
	assertReady := func(want bool) {
		t.Helper()
		health.refreshReadiness(t.Context())
		err := health.Ready(t.Context())
		if (err == nil) != want {
			t.Fatalf("ready=%t, want %t: %v", err == nil, want, err)
		}
		if !want && !errors.Is(err, errReadinessDatabase) && !errors.Is(err, errReadinessStale) {
			t.Fatal("writer error was not classified as database readiness", err)
		}
		if err := heartbeat.Check(); err != nil {
			t.Fatal("completed probes must keep liveness green", err)
		}
	}
	sample := func(at time.Duration, err error, ready bool) {
		t.Helper()
		elapsed.Store(int64(at))
		select {
		case complete <- err:
		case <-time.After(5 * time.Second):
			t.Fatal("writer probe did not complete")
		}
		waitProbe() // previous completion, publication and heartbeat renewal happened
		assertReady(ready)
	}
	sample(0, writerBusyReadinessError(5), true)
	elapsed.Store(int64(100 * time.Second))
	assertReady(true) // no second busy completion; elapsed time alone is insufficient
	sample(100*time.Second, nil, true)
	sample(110*time.Second, writerBusyReadinessError(5), true)
	sample(199*time.Second, writerBusyReadinessError(6), true)
	sample(200*time.Second, writerBusyReadinessError(5), true)
	setReadinessChecked(health.Readiness, time.Now().Add(-readinessMaxAge-time.Second))
	assertReady(false) // Persistent contention also exhausts the last-green window.
	sample(201*time.Second, nil, true)
	sample(210*time.Second, errors.New("disk unavailable"), false)
}
