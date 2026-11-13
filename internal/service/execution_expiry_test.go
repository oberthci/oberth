package service

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestReviewerExecutionTTLDoesNotExposeMinuteOldReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		s := &API{listDiagnostics: &listDiagnosticCache{}, background: &sync.WaitGroup{}, closing: &backgroundClosing{}, lifetime: ctx, stopLifetime: cancel}
		defer s.Shutdown(t.Context())
		run := model.Run{ID: "resumed-run", Status: model.RunRunning, WorkflowName: "workflow", SHA: "sha", UpdatedAt: time.Now()}
		observed := time.Now()
		s.listDiagnostic(run, "execution", func(context.Context) listDiagnosticValue {
			return listDiagnosticValue{scheduling: &model.RunScheduling{ObservedAt: observed, State: "observed", Execution: &model.ExecutionObservation{State: "observed", Phase: "Running", Reason: "Suspended", Message: model.WorkflowSuspendedMessage}}}
		})
		synctest.Wait()
		// Queue real bounded loads; each obeys its worker's two-second deadline.
		for i := 0; i < 160; i++ {
			s.listDiagnostic(model.Run{ID: fmt.Sprint("slow-", i)}, "execution", func(ctx context.Context) listDiagnosticValue { <-ctx.Done(); return listDiagnosticValue{} })
		}
		synctest.Wait()
		time.Sleep(3 * time.Second)
		refreshCalls := 0
		current := func(context.Context) listDiagnosticValue {
			refreshCalls++
			return listDiagnosticValue{scheduling: &model.RunScheduling{ObservedAt: time.Now(), Execution: &model.ExecutionObservation{State: "observed", Phase: "Running", Message: "Workflow execution is running."}}}
		}
		s.listDiagnostic(run, "execution", current)
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		got := s.listDiagnostic(run, "execution", current)
		display := run
		applyExecutionDisplay(&display, got.scheduling)
		age := int64(-1)
		if got.scheduling != nil {
			age = got.scheduling.AgeSeconds
		}
		t.Logf("elapsed=%s cached_age=%ds refresh_calls=%d displayed_reason=%q", time.Since(observed), age, refreshCalls, display.DisplayReason)
		if display.DisplayReason == model.WorkflowSuspendedMessage {
			t.Fatalf("two-second TTL still presents a resumed Workflow as suspended after %s behind bounded reads", time.Since(observed))
		}
	})
}

func TestExecutionTTLWithdrawsReasonWithoutRefreshCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		s := &API{listDiagnostics: &listDiagnosticCache{}, background: &sync.WaitGroup{}, closing: &backgroundClosing{}, lifetime: ctx, stopLifetime: cancel}
		defer s.Shutdown(t.Context())
		run := model.Run{ID: "execution", WorkflowName: "wf"}
		s.listDiagnostic(run, "execution", func(context.Context) listDiagnosticValue {
			return listDiagnosticValue{scheduling: &model.RunScheduling{ObservedAt: time.Now(), Execution: &model.ExecutionObservation{Reason: "Suspended", Message: model.WorkflowSuspendedMessage}}}
		})
		synctest.Wait()
		if got := s.listDiagnostic(run, "execution", nil); got.scheduling == nil {
			t.Fatal("fresh observation disappeared")
		}
		time.Sleep(executionDiagnosticTTL)
		if got := s.listDiagnostic(run, "execution", nil); got.scheduling != nil {
			t.Fatal("loader-free read returned expired assertion", got.scheduling)
		}
		for i := range listDiagnosticLimit {
			s.listDiagnostic(model.Run{ID: fmt.Sprint("busy-", i)}, "execution", func(ctx context.Context) listDiagnosticValue { <-ctx.Done(); return listDiagnosticValue{} })
		}
		synctest.Wait()
		var calls int
		got := s.listDiagnostic(run, "execution", func(context.Context) listDiagnosticValue { calls++; return listDiagnosticValue{} })
		if got.scheduling != nil || got.ready != nil || calls != 0 {
			t.Fatal("full queue retained expired assertion or bypassed bounds", got, calls)
		}
	})
}
