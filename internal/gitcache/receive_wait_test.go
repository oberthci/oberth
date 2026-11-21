package gitcache_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	clocktest "k8s.io/utils/clock/testing"
)

func TestConcurrentPushWaitsThroughServerTime(t *testing.T) {
	f := gittest.New(t)
	clock := clocktest.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	started := make(chan struct{})
	var admissions atomic.Int32
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.Now = clock.Now
		cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, _ *gitcache.ReceiveSource) error {
			if admissions.Add(1) == 1 {
				timer := clock.NewTimer(2600 * time.Millisecond)
				defer timer.Stop()
				close(started)
				select {
				case <-timer.C():
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
	})
	first, second := newBranchRequest(t, f, "first"), newBranchRequest(t, f, "second")
	waiting := &waitingReceive{ready: make(chan struct{})}
	second.Diagnostics = waiting
	var clientTime atomic.Bool
	second.Begin = func() { clientTime.Store(true) }
	second.End = func() { clientTime.Store(false) }
	one, two := make(chan error, 1), make(chan error, 1)
	go func() { one <- c.Receive(t.Context(), first) }()
	select {
	case <-started:
	case err := <-one:
		t.Fatal("first push did not enter admission", err)
	}
	go func() { two <- c.Receive(t.Context(), second) }()
	select {
	case <-waiting.ready:
	case err := <-two:
		t.Fatal("second push did not wait", err)
	}
	if clientTime.Load() {
		t.Error("gate wait ran the client idle clock")
	}
	clock.Step(2600 * time.Millisecond)
	for _, result := range []<-chan error{one, two} {
		if err := <-result; err != nil {
			t.Fatal("ordinary overlapping push refused", err)
		}
	}
	if admissions.Load() != 2 {
		t.Fatal("second push was not admitted")
	}
}

func TestHealthyPendingDeliveryCompletesUnderFourPushes(t *testing.T) {
	f, c, clock, record := pendingTimedDelivery(t)
	var pending, following int
	for i := range 4 {
		r := newBranchRequest(t, f, fmt.Sprintf("next-%d", i))
		started := make(chan struct{})
		r.Deliver = func(ctx context.Context, e gitcache.ReceiveEvent) ([]string, error) {
			if e.ID == record.Event.ID {
				pending++
				return nil, finishTimedDelivery(ctx, clock, started)
			}
			following++
			return nil, nil
		}
		done := make(chan error, 1)
		go func() { done <- c.Receive(t.Context(), r) }()
		if i == 0 {
			select {
			case <-started:
			case err := <-done:
				t.Fatal("pending delivery was not attempted", err)
			}
			clock.Step(1500 * time.Millisecond)
		}
		if err := <-done; err != nil {
			t.Fatal("healthy delivery blocked a later push", err)
		}
	}
	if pending != 1 || following != 4 {
		t.Fatal("delivery was cut off or repeated", pending, following)
	}
}

func TestWaitingPusherCannotCancelPendingDelivery(t *testing.T) {
	f, c, clock, _ := pendingTimedDelivery(t)
	r := newBranchRequest(t, f, "unread")
	r.Input = &unreadPush{t: t}
	started := make(chan struct{})
	var delivered atomic.Bool
	r.Deliver = func(ctx context.Context, _ gitcache.ReceiveEvent) ([]string, error) {
		err := finishTimedDelivery(ctx, clock, started)
		delivered.Store(err == nil)
		return nil, err
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Receive(ctx, r) }()
	select {
	case <-started:
	case err := <-done:
		t.Fatal("pending delivery was not attempted", err)
	}
	cancel()
	clock.Step(1500 * time.Millisecond)
	if err := <-done; !errors.Is(err, context.Canceled) || !delivered.Load() {
		t.Fatal("waiting pusher cancelled accepted delivery", err, delivered.Load())
	}
	if _, err := c.PendingReceive(t.Context(), gittest.Repository); !os.IsNotExist(err) {
		t.Fatal("completed delivery retained its record", err)
	}
}

func pendingTimedDelivery(t *testing.T) (*gittest.Fixture, *gitcache.Cache, *clocktest.FakeClock, gitcache.Reservation) {
	t.Helper()
	f := gittest.New(t)
	clock := clocktest.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.Now = clock.Now
		cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	})
	record, _ := pendingAbandonReceive(t, c, f)
	return f, c, clock, record
}

func finishTimedDelivery(ctx context.Context, clock *clocktest.FakeClock, started chan struct{}) error {
	timer := clock.NewTimer(1500 * time.Millisecond)
	defer timer.Stop()
	close(started)
	select {
	case <-timer.C():
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newBranchRequest(t *testing.T, f *gittest.Fixture, name string) gitcache.ReceiveRequest {
	t.Helper()
	r := completionRequest(t, f, f.Commit)
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/" + name, NewSHA: f.Commit})
	r.Output, r.Diagnostics = io.Discard, io.Discard
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	return r
}
