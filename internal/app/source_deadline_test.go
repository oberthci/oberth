package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestSourceOperationTimeoutMustBePositive(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for _, timeout := range []time.Duration{-1, 0, time.Nanosecond} {
		f.d.SourceTimeout = timeout
		s, err := app.New(f.d)
		if timeout <= 0 {
			expectRefusal(t, err, "config")
		} else if err != nil {
			t.Fatal(err)
		} else if err := s.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}
func TestDeliveryHonorsCallerCancellationBeforeAcceptance(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	entered := make(chan struct{})
	f.runs.BeforeAccept = func(ctx context.Context, _ stateport.BatchAcceptance) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("delivery has no source deadline")
		}
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	s := f.new(t)
	r := f.gate(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := s.Deliver(ctx, r); done <- err }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if saved(f, r.ID) || f.creates.Load() != 0 {
		t.Fatal("canceled delivery was acknowledged")
	}
	f.runs.BeforeAccept = nil
	if _, err := s.Deliver(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if !saved(f, r.ID) || f.creates.Load() != 1 || f.prepares.Load() != 1 {
		t.Fatal("original receive was not retried once")
	}
}
func TestKeepHasItsOwnDeadlineOnStartAndRecovery(t *testing.T) {
	t.Parallel()
	for _, recovering := range []bool{false, true} {
		t.Run(map[bool]string{false: "start", true: "recovery"}[recovering], func(t *testing.T) { checkKeepDeadline(t, recovering) })
	}
}
func checkKeepDeadline(t *testing.T, recovering bool) {
	t.Helper()
	f := newFixture(t)
	f.d.SourceTimeout = 45 * time.Second
	clock := newDeadlineClock()
	f.d.Deadline = clock.deadline
	entered := make(chan context.Context, 1)
	f.keeper.KeepFunc = func(ctx context.Context, _ string, _ []sourcecontract.Binding) error {
		entered <- ctx
		<-ctx.Done()
		return ctx.Err()
	}
	f.native.FindFunc = func(context.Context, stateport.Run) (nativebinding.RunBinding, bool, error) {
		t.Error("Find after failed Keep")
		return nativebinding.RunBinding{}, false, nil
	}
	s := f.new(t)
	if recovering {
		p := mustAdmit(t, s, f)
		accepted(t, f, p)
	}
	done := make(chan error, 1)
	go func() {
		if recovering {
			done <- s.Recover(t.Context())
		} else {
			_, err := s.StartRun(t.Context(), startRequest("bounded-keep", request()))
			done <- err
		}
	}()
	keep := <-entered
	if _, ok := keep.Deadline(); !ok {
		t.Fatal("Keep has no deadline")
	}
	call := keepDeadline(clock, keep)
	beforeDeadline(t, clock, call, f.d.SourceTimeout)
	err := <-done
	if recovering && err == nil {
		t.Fatal("expired recovery Keep was hidden")
	}
	if !recovering && err != nil {
		t.Fatal(err)
	}
	row, err := f.runs.Run(t.Context(), "r1")
	if err != nil || row.Status == "failed" || f.creates.Load() != 0 {
		t.Fatal("Keep timeout failed or created the run", row.Status, err)
	}
}
func keepDeadline(c *deadlineClock, ctx context.Context) deadlineCall {
	for {
		call := <-c.calls
		if call.ctx == ctx {
			return call
		}
	}
}
func TestKeepDeadlineDoesNotBoundAcceptedNativeCreation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.keeper.KeepFunc = func(ctx context.Context, _ string, _ []sourcecontract.Binding) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("Keep is unbounded")
		}
		return nil
	}
	create := f.native.CreateFunc
	f.native.CreateFunc = func(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Error("source deadline leaked to native lifetime")
		}
		return create(ctx, p)
	}
	if _, err := f.new(t).StartRun(context.Background(), startRequest("keep-only-bound", request())); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryRejectsAnAlreadyCanceledCaller(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	r := f.gate(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Deliver(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if saved(f, r.ID) || f.creates.Load() != 0 {
		t.Fatal("canceled delivery committed runs")
	}
}
