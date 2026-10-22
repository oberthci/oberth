package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/vmrunner"
)

type restartJournal struct {
	trustedRunStore
	state              vmrunner.KVMExecution
	refusals, cleanups int
}

func (journal *restartJournal) PendingKVMPreflights(context.Context) ([]vmrunner.KVMExecution, error) {
	return []vmrunner.KVMExecution{journal.state}, nil
}
func (journal *restartJournal) RecordKVMRefusal(_ context.Context, id string, refusal vmrunner.KVMRefusal) error {
	if id != journal.state.Intent.RunID || journal.state.Receipt != nil || journal.state.Refusal != nil {
		return errors.New("unexpected refusal replacement")
	}
	journal.state.Refusal = &refusal
	journal.refusals++
	return nil
}
func (journal *restartJournal) RejectKVMPreflight(_ context.Context, id string) error {
	if id != journal.state.Intent.RunID {
		return errors.New("wrong rejected intent")
	}
	journal.state.Rejected = true
	return nil
}
func (journal *restartJournal) CompleteKVMPreflightCleanup(_ context.Context, id, uid string) error {
	if id != journal.state.Intent.RunID || uid != journal.state.WorkflowUID || journal.state.Refusal == nil {
		return errors.New("cleanup without exact negative evidence")
	}
	journal.state.Cleaned = true
	journal.cleanups++
	return nil
}

type restartController struct {
	blockCleanup                         bool
	journal                              *restartJournal
	cleanupErr, absenceErr               error
	cleanupCalls, receiptCalls, uidCalls int
	cleanupDeadline                      time.Time
	cleanupBounded                       bool
}

func (controller *restartController) ConfirmTrustedKVMAbsent(context.Context, string) error {
	return controller.absenceErr
}
func (controller *restartController) TrustedKVMWorkflowUID(context.Context, argojob.Request) (string, error) {
	controller.uidCalls++
	return "", errors.New("unexpected reattachment")
}
func (controller *restartController) TrustedKVMReceipt(context.Context, argojob.Request, string) (vmrunner.KVMReceipt, error) {
	controller.receiptCalls++
	return vmrunner.KVMReceipt{}, errors.New("terminal pod is not a recovered stream")
}
func (controller *restartController) CleanupTrustedKVM(ctx context.Context, name, runID, uid string) error {
	controller.cleanupCalls++
	controller.cleanupDeadline, controller.cleanupBounded = ctx.Deadline()
	state := controller.journal.state
	if name != state.Intent.WorkflowName || runID != state.Intent.RunID || uid != state.WorkflowUID || state.Refusal == nil {
		return errors.New("unbound cleanup or missing negative receipt")
	}
	if controller.blockCleanup {
		<-ctx.Done()
		return ctx.Err()
	}
	return controller.cleanupErr
}

func TestTrustedRestartNeverReattachesOrInfersSuccessFromTerminalWorkflow(t *testing.T) {
	journal := &restartJournal{state: vmrunner.KVMExecution{Intent: vmrunner.KVMIntent{RunID: "old-run", WorkflowName: "trusted-old-run"}, WorkflowUID: "exact-workflow-uid", Submitted: true}}
	controller := &restartController{journal: journal}
	preflight := &TrustedPreflights{store: journal, controller: controller}
	ids, err := preflight.Recover(t.Context())
	if err != nil || len(ids) != 1 || ids[0] != "old-run" {
		t.Fatalf("recovery=%v %v", ids, err)
	}
	if journal.state.Refusal == nil || journal.state.Refusal.Code != "canceled" || journal.state.Receipt != nil || !journal.state.Cleaned {
		t.Fatalf("unsafe recovered evidence: %+v", journal.state)
	}
	if controller.cleanupCalls != 1 || controller.receiptCalls != 0 || controller.uidCalls != 0 {
		t.Fatal("recovery replayed or reattached execution")
	}
}

func TestTrustedRestartCleanupFailureRetainsNegativeReceiptAndObligation(t *testing.T) {
	journal := &restartJournal{state: vmrunner.KVMExecution{Intent: vmrunner.KVMIntent{RunID: "old-run", WorkflowName: "trusted-old-run"}, WorkflowUID: "exact-workflow-uid", Submitted: true}}
	failure := errors.New("owned Workflow or Pod still present")
	controller := &restartController{journal: journal, cleanupErr: failure}
	preflight := &TrustedPreflights{store: journal, controller: controller}
	ids, err := preflight.Recover(t.Context())
	if !errors.Is(err, failure) || len(ids) != 0 || journal.state.Refusal == nil || journal.state.Cleaned || journal.cleanups != 0 {
		t.Fatalf("uncertain cleanup released parent: %v %v %+v", ids, err, journal.state)
	}
	refusal := *journal.state.Refusal
	controller.cleanupErr = nil
	ids, err = preflight.Recover(t.Context())
	if err != nil || len(ids) != 1 || !journal.state.Cleaned || journal.refusals != 1 || !journal.state.Refusal.ObservedAt.Equal(refusal.ObservedAt) {
		t.Fatalf("cleanup retry lost durable refusal: %v %v", ids, err)
	}
}

func TestTrustedRestartUnboundCreateMustProveAbsence(t *testing.T) {
	for _, absent := range []bool{false, true} {
		journal := &restartJournal{state: vmrunner.KVMExecution{Intent: vmrunner.KVMIntent{RunID: "unbound", WorkflowName: "trusted-unbound"}, Submitted: true}}
		controller := &restartController{journal: journal}
		if !absent {
			controller.absenceErr = context.DeadlineExceeded
		}
		preflight := &TrustedPreflights{store: journal, controller: controller}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		ids, err := preflight.Recover(ctx)
		cancel()
		if absent {
			if err != nil || len(ids) != 1 || !journal.state.Rejected || !journal.state.Cleaned {
				t.Fatalf("proven absence not settled: %v %v", ids, err)
			}
		} else if !errors.Is(err, context.DeadlineExceeded) || len(ids) != 0 || journal.state.Cleaned || journal.state.Refusal != nil {
			t.Fatal("ambiguous create was treated as absent")
		}
	}
}

func TestTrustedRestartCleanupHonorsCallerBound(t *testing.T) {
	journal := &restartJournal{state: vmrunner.KVMExecution{Intent: vmrunner.KVMIntent{RunID: "old-run", WorkflowName: "trusted-old-run"}, WorkflowUID: "exact-workflow-uid", Submitted: true}}
	controller := &restartController{journal: journal, blockCleanup: true}
	preflight := &TrustedPreflights{store: journal, controller: controller}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	ids, err := preflight.Recover(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || len(ids) != 0 || journal.state.Cleaned || journal.state.Refusal == nil {
		t.Fatalf("cleanup deadline settled uncertain parent: ids=%v err=%v", ids, err)
	}
}

func TestTrustedRestartRecoveryHasOwnBoundWithoutCallerDeadline(t *testing.T) {
	journal := &restartJournal{state: vmrunner.KVMExecution{Intent: vmrunner.KVMIntent{RunID: "old-run", WorkflowName: "trusted-old-run"}, WorkflowUID: "exact-workflow-uid", Submitted: true}}
	controller := &restartController{journal: journal}
	preflight := &TrustedPreflights{store: journal, controller: controller}
	if _, ok := t.Context().Deadline(); ok {
		t.Fatal("test context unexpectedly bounded")
	}
	before := time.Now()
	ids, err := preflight.Recover(t.Context())
	after := time.Now()
	if err != nil || len(ids) != 1 || controller.cleanupCalls != 1 {
		t.Fatalf("recovery=%v %v", ids, err)
	}
	// Startup has no deadline of its own; a stuck cleanup must still end within
	// the two-minute per-execution bound rather than hold startup indefinitely.
	if !controller.cleanupBounded || controller.cleanupDeadline.Before(before.Add(trustedRecoveryBound)) || controller.cleanupDeadline.After(after.Add(trustedRecoveryBound)) || trustedRecoveryBound != 2*time.Minute {
		t.Fatalf("cleanup deadline=%v bounded=%v, want two minutes after start", controller.cleanupDeadline, controller.cleanupBounded)
	}
}
