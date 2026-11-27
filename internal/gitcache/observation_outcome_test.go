package gitcache

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"k8s.io/utils/clock"
)

func TestReceiveWaitPreservesTerminalOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, code, reason string
		err                error
	}{
		{"timeout", "cancelled", "forge-timeout", unavailable("forge", context.DeadlineExceeded)},
		{"refused", "unavailable", "forge-refused", unavailable("forge", errors.New(gitssh.ReasonIdentityRefused))},
		{"success", "ok", "none", nil},
		{"contention", "forbidden", "contention", pushBusy("origin/team/widget")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, observation := sourcedelivery.Observe(t.Context(), "receive")
				slot := make(chan struct{}, 1)
				slot <- struct{}{}
				done := make(chan error, 1)
				go func() { done <- acquireReceive(ctx, slot, io.Discard) }()
				synctest.Wait()
				(clock.RealClock{}).Sleep(time.Millisecond)
				<-slot
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				<-slot
				s := finishObservation(observation, tc.err)
				if s.Wait[sourcedelivery.ReceiveSlot] != time.Millisecond || !s.Blocked || s.Phase != sourcedelivery.ReceiveSlot {
					t.Fatal("earlier receive wait was lost", s)
				}
				if s.Reason != tc.reason || s.Code != tc.code {
					t.Fatalf("terminal outcome replaced after receive wait: code=%q reason=%q", s.Code, s.Reason)
				}
			})
		})
	}
}
