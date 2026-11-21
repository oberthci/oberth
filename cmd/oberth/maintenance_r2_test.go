package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

type maintenanceLog struct{ lines chan string }

func (l maintenanceLog) Write(data []byte) (int, error) {
	l.lines <- string(data)
	return len(data), nil
}

type maintenanceJob struct {
	h              *health
	clock          *fake.Clock
	out            maintenanceLog
	done           chan error
	cancel         context.CancelFunc
	kind, metric   string
	interval, next time.Duration
}

func TestFailedMaintenanceTicksStayLiveAndRetry(t *testing.T) {
	for _, kind := range []string{"retention", "cache"} {
		t.Run(kind, func(t *testing.T) { maintenanceFailureCase(t, kind) })
	}
}
func maintenanceFailureCase(t *testing.T, kind string) {
	t.Helper()
	job := startMaintenanceJob(t, kind)
	for count := 1; count <= 2; count++ {
		job.tick(t, count)
	}
	select {
	case line := <-job.out.lines:
		t.Fatalf("duplicate tick log %q", line)
	default:
	}
	job.cancel()
	if err := <-job.done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if job.clock.ActiveTickers() != 0 {
		t.Fatal("maintenance ticker leaked")
	}
}
func startMaintenanceJob(t *testing.T, kind string) *maintenanceJob {
	t.Helper()
	clock := fake.NewClock()
	h := &health{clock: serverClock{clock.Now, clock.Ticker}, readySignal: make(chan struct{}), validLeaf: func() bool { return true }, success: clock.Now()}
	h.setStarted(true)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	job := &maintenanceJob{h: h, clock: clock, out: maintenanceLog{make(chan string, 4)}, done: make(chan error, 1), cancel: cancel, kind: kind, interval: 5 * time.Minute, next: 24 * time.Hour, metric: "oberth_event_retention_failures_total"}
	fail := func(context.Context) error { return fmt.Errorf("repository acme/app.git: storage busy") }
	if kind == "cache" {
		job.interval, job.next = time.Hour, time.Hour
		job.metric = "oberth_git_maintenance_failures_total"
		c := &composition{clock: h.clock, health: h, cacheGC: fail, diagnostic: job.out}
		go func() { job.done <- c.maintainCache(ctx) }()
	} else {
		go func() {
			job.done <- h.retainEvents(ctx, func(ctx context.Context, _, _ time.Time) error { return fail(ctx) }, job.out)
		}()
	}
	if d := <-clock.Started; d != job.interval {
		t.Fatalf("first interval %v", d)
	}
	return job
}
func (job *maintenanceJob) tick(t *testing.T, count int) {
	t.Helper()
	job.h.mu.Lock()
	job.h.success = job.clock.Now().Add(job.interval)
	job.h.mu.Unlock()
	job.clock.Advance(job.interval)
	select {
	case line := <-job.out.lines:
		assertMaintenanceLog(t, line)
	case err := <-job.done:
		t.Fatalf("failed tick killed lifetime: %v", err)
	case <-time.After(time.Second):
		t.Fatal("failed tick was not logged")
	}
	if job.h.ready(t.Context()) != nil || !strings.Contains(job.h.metrics(t.Context()), fmt.Sprintf("%s %d\n", job.metric, count)) {
		t.Fatal("failure lost readiness or metric")
	}
	if job.kind == "retention" && count == 1 {
		if d := <-job.clock.Started; d != job.next {
			t.Fatal("wrong repeat interval")
		}
	}
	job.interval = job.next
}
func assertMaintenanceLog(t *testing.T, line string) {
	t.Helper()
	if !strings.Contains(line, "acme/app.git") || !strings.Contains(line, "next") || strings.Count(line, "\n") != 1 {
		t.Fatalf("unhelpful tick log %q", line)
	}
}

func TestOpenSSHSessionEndsWithItsPartLifetime(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, ended, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	c := &composition{ssh: func(session context.Context, _ net.Listener) error {
		close(entered)
		select {
		case <-session.Done():
			close(ended)
			return session.Err()
		case <-release:
			return nil
		}
	}}
	listener := &memoryListener{incoming: make(chan net.Conn), closed: make(chan struct{})}
	part := sshPart(c, func(context.Context, string, string) (net.Listener, error) { return listener, nil })
	life := newLifecycle(ctx, []Part{part})
	if err := life.start(); err != nil {
		t.Fatal(err)
	}
	<-entered
	stop, stopCancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer stopCancel()
	if err := life.stop(stop); err != nil {
		t.Fatalf("SSH session ignored part cancellation: %v", err)
	}
	select {
	case <-ended:
	default:
		t.Fatal("open SSH session survived part stop")
	}
}

func TestHTTPFailureInterruptsBlockedStartup(t *testing.T) {
	f := fixtureServer(t)
	entered := make(chan struct{})
	original := f.deps.constructors.NativeNew
	f.deps.constructors.NativeNew = func(ctx context.Context, c *composition) (Part, error) {
		p, err := original(ctx, c)
		p.Ready = func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }
		return p, err
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("server did not stop after failed-start test")
		}
	})
	go func() { defer close(finished); done <- serve(ctx, f.config, f.deps, f.trace) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("startup not reached")
	}
	_ = f.listener(f.config.Listen.HTTPS).Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("listener failure not preserved: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP failure left startup blocked")
	}
}

func TestHealthStartupAndShutdownRemedies(t *testing.T) {
	for reason, want := range map[string]string{"starting": "Wait for startup to finish, then retry.", "stopping": "The server is restarting; retry shortly."} {
		var problem *publicerr.Error
		if err := healthError(reason); !errors.As(err, &problem) || problem.Remedy() != want {
			t.Fatalf("%s: %v", reason, err)
		}
	}
}

func TestFailedGitTickKeepsHTTPServing(t *testing.T) {
	f := fixtureServer(t)
	called := make(chan struct{}, 1)
	original := f.deps.constructors.CacheNew
	f.deps.constructors.CacheNew = func(ctx context.Context, c *composition) (Part, error) {
		p, err := original(ctx, c)
		c.cacheGC = func(context.Context) error { called <- struct{}{}; return errors.New("repository acme/app.git: busy") }
		return p, err
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := f.start(ctx)
	intervals := map[time.Duration]bool{}
	for len(intervals) < 3 {
		intervals[<-f.clock.Started] = true
	}
	f.clock.Advance(time.Hour)
	<-called
	// The source handler proves the listeners and component lifetimes still
	// serve after failure, independently of the concurrently waking sampler.
	if status := readStatus(t, f.client(f.config.Listen.Source), "/work"); status != http.StatusNoContent {
		t.Fatalf("live status %d", status)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("failed Git tick stopped the server: %v", err)
	}
}
