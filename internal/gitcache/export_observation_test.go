package gitcache

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/sourcedelivery"
	"k8s.io/utils/clock"
)

func TestLatestCheckedStageWaitWins(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"complete", nil},
		{"cancel", context.Canceled},
		{"deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				checkLatestStageWait(t, tc.cause)
			})
		})
	}
}

func checkLatestStageWait(t *testing.T, cause error) {
	t.Helper()
	f := newExportWaitFixture(t)
	ctx, observation := sourcedelivery.Observe(t.Context(), "load")
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)
	built := make(chan struct{})
	f.cache.config.Observe = func(sourcedelivery.Summary) {
		f.cache.repositoryGate(f.path) <- struct{}{}
		close(built)
	}
	f.cache.export <- struct{}{}
	done := make(chan exportWaitResult, 1)
	go func() { s, err := f.cache.Stage(ctx, f.binding); done <- exportWaitResult{s, err} }()
	synctest.Wait()
	<-f.cache.export
	<-built
	synctest.Wait()
	(clock.RealClock{}).Sleep(time.Millisecond)
	if s := observation.Finish("", nil); s.Phase != sourcedelivery.Repository {
		t.Fatal("fixture did not reach checked-stage wait", s)
	}
	if cause != nil {
		cancel(cause)
	} else {
		f.cache.unlock(f.path)
	}
	r := <-done
	if cause != nil {
		f.cache.unlock(f.path)
	}
	assertCheckedStageResult(t, r, cause)
	s := observation.Finish("", r.err)
	if s.Phase != sourcedelivery.Repository || s.Wait[sourcedelivery.Repository] != time.Millisecond || s.Cancelled != (cause != nil) {
		t.Fatal("newer checked-stage outcome overwritten by build history", s)
	}
}

func assertCheckedStageResult(t *testing.T, r exportWaitResult, cause error) {
	t.Helper()
	if cause != nil {
		if !errors.Is(r.err, cause) || r.stage.Body != nil {
			t.Fatal("checked-stage cancellation lost", r.err)
		}
	} else {
		if r.err != nil || !r.stage.Waited {
			t.Fatal("checked-stage wait lost its authorization signal", r.err)
		}
		_ = r.stage.Body.Close()
	}
}

func TestCancelledArchiveWaitInheritsBuildPhase(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newExportWaitFixture(t)
		ctx, observation := sourcedelivery.Observe(t.Context(), "load")
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		f.cache.export <- struct{}{}
		done := make(chan error, 1)
		go func() { _, err := f.cache.Stage(ctx, f.binding); done <- err }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if s := observation.Finish("", context.Canceled); !s.Blocked || s.Phase != sourcedelivery.Export {
			t.Fatal("early cancellation lost the shared build wait", s)
		}
		<-f.cache.export
		f.cache.staged.workers.Wait()
	})
}
