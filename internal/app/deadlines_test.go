package app_test

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	clocktest "k8s.io/utils/clock/testing"
)

type deadlineCall struct {
	ctx      context.Context
	duration time.Duration
}
type deadlineClock struct {
	clock *clocktest.FakeClock
	calls chan deadlineCall
}

func newDeadlineClock() *deadlineClock {
	return &deadlineClock{clock: clocktest.NewFakeClock(time.Unix(0, 0)), calls: make(chan deadlineCall, 8)}
}
func (c *deadlineClock) deadline(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	timer := c.clock.AfterFunc(d, func() { cancel(context.DeadlineExceeded) })
	bounded := deadlineContext{Context: ctx, until: c.clock.Now().Add(d)}
	c.calls <- deadlineCall{bounded, d}
	return bounded, func() { timer.Stop(); cancel(context.Canceled) }
}
func beforeDeadline(t *testing.T, c *deadlineClock, call deadlineCall, want time.Duration) {
	t.Helper()
	if call.duration != want {
		t.Fatalf("deadline %s, want %s", call.duration, want)
	}
	c.clock.Step(want - time.Nanosecond)
	select {
	case <-call.ctx.Done():
		t.Fatal("deadline fired early")
	default:
	}
	c.clock.Step(time.Nanosecond)
	if !errors.Is(context.Cause(call.ctx), context.DeadlineExceeded) {
		t.Fatal("deadline did not fire at its bound")
	}
}
func TestGateDeadlineActuallyCancelsAdmissionAtThirtySeconds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	clock := newDeadlineClock()
	f.d.Deadline = clock.deadline
	entered := make(chan struct{})
	f.source.Read = func(ctx context.Context, _ gitport.Revision, _ string) (gitport.File, error) {
		close(entered)
		<-ctx.Done()
		return gitport.File{}, context.Cause(ctx)
	}
	s := f.new(t)
	r := receive()
	r.Source = f.source
	done := make(chan error, 1)
	go func() { _, err := s.Gate(context.Background(), r); done <- err }()
	call := <-clock.calls
	<-entered
	beforeDeadline(t, clock, call, 30*time.Second)
	expectRefusal(t, <-done, "timeout")
	if saved(f, r.ID) {
		t.Fatal("expired gate saved a run")
	}
}
func TestGatePostcheckRejectsExpiryAfterLastPreparedRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.runs.DecideFunc = func(_ context.Context, u service.RefUpdate) (service.Decision, error) {
		if u.Ref == "refs/heads/last" {
			cancel()
			return decision(u, "parked"), nil
		}
		return decision(u, ""), nil
	}
	s := f.new(t)
	r := receive()
	last := r.Updates[0]
	last.Request.RunID = "last"
	last.Request.Ref.Name = "refs/heads/last"
	r.Updates = append(r.Updates, last)
	r.Source = f.source
	_, err := s.Gate(ctx, r)
	expectRefusal(t, err, "timeout")
	if f.prepares.Load() != 1 {
		t.Fatal("expiry did not occur after the first seal")
	}
	if app.ReceiveStashSize(s) != 0 {
		t.Fatal("expired admission was stashed")
	}
}
func TestDeliveryDoesNotAcknowledgeBeforeAcceptance(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	clock := newDeadlineClock()
	f.d.Deadline = clock.deadline
	entered := make(chan struct{})
	release := make(chan struct{})
	f.runs.BeforeAccept = func(ctx context.Context, _ stateport.BatchAcceptance) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s := f.new(t)
	r := f.gate(t, s)
	<-clock.calls
	done := make(chan error, 1)
	go func() { _, err := s.Deliver(context.Background(), r); done <- err }()
	<-entered
	call := <-clock.calls
	beforeDeadline(t, clock, call, f.d.SourceTimeout)
	if err := <-done; err == nil {
		t.Fatal("acknowledged unsaved runs")
	}
	if saved(f, r.ID) {
		t.Fatal("blocked transaction was durable")
	}
	f.runs.BeforeAccept = nil
	f.d.Deadline = context.WithTimeout
	s = f.new(t)
	if _, err := s.Deliver(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if !saved(f, r.ID) {
		t.Fatal("retry returned without a durable receive")
	}
}

func TestCrashBeforeAcceptanceRemainsReplayable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.runs.BeforeAccept = func(context.Context, stateport.BatchAcceptance) error {
		return errors.New("transaction did not commit")
	}
	s := f.new(t)
	r := f.gate(t, s)
	if _, err := s.Deliver(context.Background(), r); err == nil {
		t.Fatal("unsaved transaction acknowledged")
	}
	closeSubmitter(t, s)
	if saved(f, r.ID) {
		t.Fatal("failed transaction saved receipt")
	}
	f.runs.BeforeAccept = nil
	s = f.new(t)
	if _, err := s.Deliver(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !saved(f, r.ID) {
		t.Fatal("push was not replayed")
	}
}
func TestCrashAfterAcceptanceNeverSubmitsAnotherWorkflow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	p := mustAdmit(t, s, f)
	accepted(t, f, p)
	closeSubmitter(t, s)
	next := f.new(t)
	if err := next.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.creates.Load() != 0 {
		t.Fatal("restart submitted a replacement")
	}
}
func TestDeliveryWaitExpiresOnlyAfterSavedAcceptance(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	clock := newDeadlineClock()
	f.d.Deadline = clock.deadline
	entered := make(chan struct{})
	release := make(chan struct{})
	f.keeper.KeepFunc = func(ctx context.Context, _ string, _ []sourcecontract.Binding) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s := f.new(t)
	releaseOnce := sync.Once{}
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	r := f.gate(t, s)
	<-clock.calls
	type response struct {
		result app.PushResult
		err    error
	}
	done := make(chan response, 1)
	go func() { result, err := s.Deliver(context.Background(), r); done <- response{result, err} }()
	call := feedbackDeadline(clock)
	<-entered
	if !saved(f, r.ID) {
		t.Fatal("feedback timer preceded durable acceptance")
	}
	beforeDeadline(t, clock, call, 10*time.Second)
	got := <-done
	if got.err != nil || got.result.Outcomes[0].State != "accepted" || f.creates.Load() != 0 {
		t.Fatal(got)
	}
	releaseOnce.Do(func() { close(release) })
	closeSubmitter(t, s)
	if _, bound := f.runs.Workflow(t.Context(), "r1"); !bound {
		t.Fatal("feedback timeout cancelled submission")
	}
}
func TestDeliverSubmissionSurvivesCallerCancellation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	f.keeper.KeepFunc = func(ctx context.Context, _ string, _ []sourcecontract.Binding) error {
		close(entered)
		select {
		case <-release:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s := f.new(t)
	r := f.gate(t, s)
	caller, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.Deliver(caller, r); done <- err }()
	<-entered
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	close(release)
	closeSubmitter(t, s)
	if _, bound := f.runs.Workflow(t.Context(), "r1"); !bound {
		t.Fatal("caller cancelled accepted work")
	}
}
func TestCloseDrainsInFlightSubmission(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	closing := make(chan error, 1)
	f.native.CreateFunc = func(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		close(entered)
		select {
		case <-release:
			return binding(p.View()), ctx.Err()
		case <-ctx.Done():
			return nativebinding.RunBinding{}, ctx.Err()
		}
	}
	s := f.new(t)
	r := f.gate(t, s)
	caller, cancel := context.WithCancel(context.Background())
	defer cancel()
	response := make(chan error, 1)
	go func() { _, err := s.Deliver(caller, r); response <- err }()
	<-entered
	cancel()
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	go func() { closing <- s.Close(context.Background()) }()
	select {
	case err := <-closing:
		t.Fatalf("did not drain: %v", err)
	default:
	}
	close(release)
	if err := <-closing; err != nil {
		t.Fatal(err)
	}
	if _, bound := f.runs.Workflow(t.Context(), "r1"); !bound {
		t.Fatal("shutdown lost native identity")
	}
}
func TestCloseHonorsServerTerminationDeadline(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	unwound := make(chan struct{})
	f.keeper.KeepFunc = func(ctx context.Context, _ string, _ []sourcecontract.Binding) error {
		close(entered)
		defer close(unwound)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s := f.new(t)
	t.Cleanup(func() { close(release) })
	r := f.gate(t, s)
	caller, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	response := make(chan error, 1)
	go func() { _, err := s.Deliver(caller, r); response <- err }()
	<-entered
	cancelCaller()
	if err := <-response; err != nil {
		t.Fatal(err)
	}
	clock := newDeadlineClock()
	shutdown, cancel := clock.deadline(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Close(shutdown) }()
	call := <-clock.calls
	beforeDeadline(t, clock, call, 5*time.Second)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("termination deadline ignored", err)
	}
	<-unwound
}

func TestTerminationDuringAcceptanceLeavesReplayablePush(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	entered := make(chan struct{})
	caller, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	f.runs.BeforeAccept = func(ctx context.Context, _ stateport.BatchAcceptance) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	s := f.new(t)
	r := f.gate(t, s)
	done := make(chan error, 1)
	go func() { _, err := s.Deliver(caller, r); done <- err }()
	<-entered
	shutdown, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(shutdown); err == nil {
		t.Fatal("termination deadline hidden")
	}
	if err := <-done; err == nil {
		t.Fatal("unsaved receive was acknowledged")
	}
	if saved(f, r.ID) {
		t.Fatal("uncommitted receive saved")
	}
	f.runs.BeforeAccept = nil
	restarted := f.new(t)
	if _, err := restarted.Deliver(context.Background(), r); err != nil {
		t.Fatal("push was not replayable", err)
	}
	if _, bound := f.runs.Workflow(t.Context(), "r1"); !bound {
		t.Fatal("replay did not create the run")
	}
}

type deadlineContext struct {
	context.Context
	until time.Time
}

func (c deadlineContext) Deadline() (time.Time, bool) {
	if d, ok := c.Context.Deadline(); ok && d.Before(c.until) {
		return d, true
	}
	return c.until, true
}
func feedbackDeadline(c *deadlineClock) deadlineCall {
	for {
		call := <-c.calls
		if call.duration == 10*time.Second {
			return call
		}
	}
}
