package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWiringRecoveryPrecedesPushReplayAndServing(t *testing.T) {
	f := fixtureServer(t)
	wiring := newRecoveryStep(f.trace, "wiring recovery")
	replay := newRecoveryStep(f.trace, "pending push replay")
	installRecoverySteps(f, wiring.run, replay.run)
	f.source = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.trace.record("serve work")
		w.WriteHeader(204)
	})
	run := startRecoveryServer(t, f)
	awaitRecoveryStep(t, run, wiring, replay.entered)
	assertRecoveryGatesWork(t, f)
	close(wiring.release)
	awaitRecoveryStep(t, run, replay, nil)
	assertRecoveryGatesWork(t, f)
	close(replay.release)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-f.trace.ready:
	case <-run.done:
		t.Fatalf("startup failed: %v", run.err)
	case <-timer.C:
		t.Fatal("recovered server never became ready")
	}
	<-f.sshContext
	if status := readStatus(t, f.client(f.config.Listen.Source), "/work"); status != 204 {
		t.Fatalf("recovered source did not serve: %d", status)
	}
	events := f.trace.snapshot()
	previous := -1
	for _, event := range []string{"wiring recovery complete", "pending push replay begin", "pending push replay complete", "bind " + f.config.Listen.SSH, "serve work"} {
		index := slices.Index(events, event)
		if index <= previous {
			t.Fatalf("startup order at %q: %v", event, events)
		}
		previous = index
	}
}

func TestRecoveryFailurePreventsReplayOrServing(t *testing.T) {
	for _, stage := range []string{"wiring", "replay"} {
		t.Run(stage, func(t *testing.T) {
			f := fixtureServer(t)
			failure := errors.New(stage + " recovery refused")
			recoverWiring := func(context.Context) error { f.trace.record("recovered wiring"); return nil }
			replay := func(context.Context) error { f.trace.record("replayed pushes"); return failure }
			if stage == "wiring" {
				recoverWiring = func(context.Context) error { return failure }
			}
			installRecoverySteps(f, recoverWiring, replay)
			run := startRecoveryServer(t, f)
			awaitRecoveryFailure(t, run)
			if !errors.Is(run.err, failure) {
				t.Fatalf("recovery refusal lost: %v", run.err)
			}
			if stage == "wiring" && slices.Contains(f.trace.snapshot(), "replayed pushes") {
				t.Fatal("pushes replayed after wiring recovery failed")
			}
			assertRecoveryNeverServed(t, f)
		})
	}
}

func TestRecoveryPortsAreRequired(t *testing.T) {
	for _, port := range []string{"app.Recover", "gitcache.ReplayPending"} {
		t.Run(port, func(t *testing.T) {
			f := fixtureServer(t)
			wiring := func(context.Context) error { return nil }
			replay := wiring
			if port == "app.Recover" {
				wiring = nil
			} else {
				replay = nil
			}
			installRecoverySteps(f, wiring, replay)
			run := startRecoveryServer(t, f)
			awaitRecoveryFailure(t, run)
			if run.err == nil || !strings.Contains(run.err.Error(), port) {
				t.Fatalf("missing %s not refused: %v", port, run.err)
			}
			assertRecoveryNeverServed(t, f)
		})
	}
}

func installRecoverySteps(f *serverFixture, wiring, replay func(context.Context) error) {
	buildWiring, buildSource := f.deps.constructors.WiringNew, f.deps.constructors.SourceNew
	f.deps.constructors.WiringNew = func(ctx context.Context, c *composition) (Part, error) {
		p, err := buildWiring(ctx, c)
		p.Recover = wiring
		return p, err
	}
	f.deps.constructors.SourceNew = func(ctx context.Context, c *composition) (Part, error) {
		p, err := buildSource(ctx, c)
		c.replayPending = replay
		return p, err
	}
}

type recoveryStep struct {
	trace            *serverTrace
	name             string
	entered, release chan struct{}
}

func newRecoveryStep(trace *serverTrace, name string) *recoveryStep {
	return &recoveryStep{trace, name, make(chan struct{}), make(chan struct{})}
}
func (s *recoveryStep) run(ctx context.Context) error {
	s.trace.record(s.name + " begin")
	close(s.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		s.trace.record(s.name + " complete")
		return nil
	}
}

type recoveryServer struct {
	done chan struct{}
	err  error
}

func startRecoveryServer(t *testing.T, f *serverFixture) *recoveryServer {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	run := &recoveryServer{done: make(chan struct{})}
	go func() { run.err = serve(ctx, f.config, f.deps, f.trace); close(run.done) }()
	t.Cleanup(func() { cancel(); <-run.done })
	return run
}
func awaitRecoveryStep(t *testing.T, run *recoveryServer, step *recoveryStep, premature <-chan struct{}) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-step.entered:
	case <-premature:
		t.Fatalf("pending pushes replayed before %s", step.name)
	case <-run.done:
		t.Fatalf("server stopped before %s: %v", step.name, run.err)
	case <-timer.C:
		t.Fatalf("server never reached %s", step.name)
	}
}
func awaitRecoveryFailure(t *testing.T, run *recoveryServer) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-run.done:
	case <-timer.C:
		t.Fatal("server did not refuse incomplete or failed recovery")
	}
}
func assertRecoveryGatesWork(t *testing.T, f *serverFixture) {
	t.Helper()
	if f.listener(f.config.Listen.SSH) != nil {
		t.Fatal("SSH bound before recovery completed")
	}
	select {
	case <-f.trace.ready:
		t.Fatal("server marked ready before recovery completed")
	default:
	}
	if readStatus(t, f.client(f.config.Listen.HTTPS), "/readyz") != 503 || readStatus(t, f.client(f.config.Listen.Source), "/work") != 503 {
		t.Fatal("work or readiness exposed before recovery completed")
	}
	if readStatus(t, f.client(f.config.Listen.HTTPS), "/livez") != 200 {
		t.Fatal("recovery hid liveness")
	}
}
func assertRecoveryNeverServed(t *testing.T, f *serverFixture) {
	t.Helper()
	if f.listener(f.config.Listen.SSH) != nil || slices.Contains(f.trace.snapshot(), "ssh run") {
		t.Fatal("SSH served despite failed recovery")
	}
	select {
	case <-f.trace.ready:
		t.Fatal("failed recovery reported ready")
	default:
	}
	if f.c.store.Readiness(t.Context()) == nil {
		t.Fatal("failed recovery left storage open")
	}
}
