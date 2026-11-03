package service

import (
	"context"
	"log"
	"time"

	"github.com/oberthci/oberth/internal/vmrunner"
)

const preflightCleanupInterval = 5 * time.Second

// Terminal preflights release admission before their Kubernetes objects are
// necessarily gone. Reconcile those journals separately from dispatch, and
// keep retrying absence readback without requiring an owner restart.
func (scheduler *Scheduler) runTerminalPreflightCleanup(ctx context.Context) {
	journal, ok := scheduler.store.(vmrunner.KVMJournal)
	if !ok {
		return
	}
	recoverer, ok := scheduler.trusted.(interface {
		RecoverRun(context.Context, string) error
	})
	if !ok {
		return
	}
	ticker := time.NewTicker(preflightCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := scheduler.awaitMutation(ctx); err != nil {
			return
		}
		pending, err := retryDatabase(ctx, "PendingKVMPreflights", journal.PendingKVMPreflights)
		if err != nil {
			log.Printf("WARN: terminal preflight cleanup inventory unavailable: %v", err)
			continue
		}
		for _, execution := range pending {
			if ctx.Err() != nil {
				return
			}
			if execution.Cleaned {
				continue
			}
			id := execution.Intent.RunID
			run, err := scheduler.database().Run(ctx, id)
			if err != nil {
				log.Printf("WARN: terminal preflight cleanup parent unavailable run=%s: %v", id, err)
				continue
			}
			// Main Workflow submission follows confirmed preflight cleanup, so
			// a parent with WorkflowName cannot own a pending preflight journal.
			if !run.Status.Terminal() || run.WorkflowName != "" {
				continue // Never enumerate/cancel unrelated live executions.
			}
			scheduler.evidenceMu.Lock()
			active := scheduler.evidenceExecutions[id] > 0
			scheduler.evidenceMu.Unlock()
			if active {
				continue // A cancelled owner may still be creating or cleaning up.
			}
			// The durable terminal parent prevents a new execution from starting.
			// Protect this cleanup writer from evidence purge until it returns.
			finishEvidence := scheduler.beginEvidenceExecution(id)
			recoveryCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			err = scheduler.RetryRecovery(recoveryCtx, "RecoverTerminalPreflight", func(ctx context.Context) error {
				return recoverer.RecoverRun(ctx, id)
			})
			cancel()
			if err == nil {
				err = scheduler.database().CompleteSupersededRunCancellationWithoutJob(ctx, id)
			}
			finishEvidence()
			if err != nil {
				log.Printf("WARN: terminal preflight cleanup remains pending run=%s: %v", id, err)
				continue
			}
			scheduler.signals.NotifyRun(id)
			scheduler.signalQueue()
		}
	}
}
