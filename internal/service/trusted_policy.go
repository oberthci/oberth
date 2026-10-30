package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

// TrustedPreflight is deliberately separate from release delivery. Execute
// finishes credential-free testing AND cleanup before the main Job is recorded.
type TrustedPreflight interface {
	// Supports requires an exact suite/guest pair in the current operator recipes.
	Supports(vmrunner.Contract) error
	RecipeAllowed(vmrunner.KVMRecipe) error
	Execute(context.Context, JobRequest, vmrunner.Requirement, io.Writer) error
	Recover(context.Context) ([]string, error)
}

func (scheduler *Scheduler) trustedRequirement(ctx context.Context, repository model.Repository, runID, candidateSHA string) (vmrunner.Requirement, error) {
	requirement, policyErr := scheduler.resolveTrustedRequirement(ctx, repository, candidateSHA)
	if previous := requirement.RotatedFrom; previous != nil {
		if runID == "" {
			return requirement, errors.Join(policyErr, errors.New("audit trusted test pin rotation: run identity is required"))
		}
		action := "trusted.pin_rotation.authorized"
		fields := map[string]string{
			"run_id":        runID,
			"candidate_sha": requirement.CandidateSHA, "baseline_sha": requirement.BaselineSHA,
			"from_suite_sha256": previous.SuiteSHA256, "from_guest_sha256": previous.GuestSHA256,
			"to_suite_sha256": requirement.Contract.SuiteSHA256, "to_guest_sha256": requirement.Contract.GuestSHA256,
		}
		if policyErr != nil {
			action, fields["reason"] = "trusted.pin_rotation.refused", policyErr.Error()
		}
		details, err := json.Marshal(fields)
		if err == nil {
			_, err = (databaseAuditor{scheduler.auditor}).AppendAuditAction(ctx, model.AuditActionSpec{
				Actor: "scheduler", Action: action, ResourceType: "repository",
				ResourceID: strconv.FormatInt(repository.ID, 10), Details: string(details),
			})
		}
		if err != nil {
			return requirement, errors.Join(policyErr, fmt.Errorf("audit trusted test pin rotation: %w", err))
		}
	}
	return requirement, policyErr
}

// resolveTrustedRequirement refreshes and validates policy without auditing.
// All progress boundaries, including capacity retries, use trustedRequirement
// to durably record decision changes without caching any policy authority.
func (scheduler *Scheduler) resolveTrustedRequirement(ctx context.Context, repository model.Repository, candidateSHA string) (vmrunner.Requirement, error) {
	upstream, err := scheduler.database().Upstream(ctx, repository.UpstreamID)
	if err != nil {
		return vmrunner.Requirement{}, fmt.Errorf("load trusted test policy upstream: %w", err)
	}
	var authorizeRotation func(vmrunner.Contract) error
	if scheduler.trusted != nil {
		// Supports resolves the exact suite/guest pair against the executor's
		// current operator-owned recipes, including at publication boundaries.
		authorizeRotation = scheduler.trusted.Supports
	}
	resume := suspendDispatch(ctx)
	requirement, err := vmrunner.ResolveRequirement(ctx, scheduler.git, upstream.QualifiedRepo(repository.Name), candidateSHA, authorizeRotation)
	resume()
	if err != nil {
		return requirement, err
	}
	if requirement.Contract != nil {
		if scheduler.trusted == nil {
			return requirement, refuse(ErrUnavailable, "trusted test profile %s requires a protected suite executor and verified result evidence", requirement.Contract.Profile)
		}
		if _, ok := scheduler.store.(vmrunner.KVMJournal); !ok {
			return requirement, refuse(ErrUnavailable, "trusted test journal unavailable")
		}
		if err = scheduler.trusted.Supports(*requirement.Contract); err != nil {
			return requirement, err
		}
	}
	return requirement, nil
}

func (scheduler *Scheduler) trustedReceipt(ctx context.Context, repository model.Repository, run model.Run, requirement vmrunner.Requirement) error {
	if requirement.Contract == nil {
		return nil
	}
	journal, ok := scheduler.store.(vmrunner.KVMJournal)
	if !ok {
		return fmt.Errorf("trusted test journal unavailable")
	}
	execution, err := retryDatabase(ctx, "KVMPreflight", func(ctx context.Context) (vmrunner.KVMExecution, error) { return journal.KVMPreflight(ctx, run.ID) })
	if err != nil {
		return fmt.Errorf("read trusted test receipt: %w", err)
	}
	intent := execution.Intent
	if err := scheduler.trusted.RecipeAllowed(intent.Recipe); err != nil {
		return err
	}
	if !execution.Cleaned || execution.Rejected || execution.Refusal != nil || execution.Receipt == nil || execution.WorkflowUID == "" || execution.WorkflowUID != execution.Receipt.WorkflowUID || intent.RunID != run.ID || intent.RepoID != repository.ID || intent.CandidateSHA != requirement.CandidateSHA || intent.BaselineSHA != requirement.BaselineSHA || vmrunner.ContractDigest(&intent.Contract) != vmrunner.ContractDigest(requirement.Contract) {
		return fmt.Errorf("trusted test receipt does not match publication policy")
	}
	upstream, err := scheduler.database().Upstream(ctx, repository.UpstreamID)
	if err != nil {
		return err
	}
	if intent.Repository != upstream.QualifiedRepo(repository.Name) {
		return fmt.Errorf("trusted test receipt repository mismatch")
	}
	return execution.Receipt.Validate(intent)
}

func (scheduler *Scheduler) requirePublicationTestPolicy(ctx context.Context, repository model.Repository, publication model.Publication) error {
	candidateSHA := publication.ResultSHA
	var run model.Run
	if publication.RunID != "" {
		var err error
		run, err = scheduler.database().Run(ctx, publication.RunID)
		if err != nil {
			return fmt.Errorf("load trusted test publication run: %w", err)
		}
		if run.RepoID != repository.ID {
			return fmt.Errorf("trusted test publication run does not match repository")
		}
		candidateSHA = run.TestedSHA
		if candidateSHA == "" {
			candidateSHA = run.SHA
		}
	}
	var sourceErr error
	if run.ID == "" {
		lookup, ok := scheduler.store.(interface {
			LatestRunForSHA(context.Context, int64, string) (model.Run, error)
		})
		if !ok {
			sourceErr = fmt.Errorf("trusted fast-forward source run unavailable")
		} else {
			run, sourceErr = retryDatabase(ctx, "LatestRunForSHA", func(ctx context.Context) (model.Run, error) {
				return lookup.LatestRunForSHA(ctx, repository.ID, candidateSHA)
			})
		}
	}
	requirement, err := scheduler.trustedRequirement(ctx, repository, run.ID, candidateSHA)
	if err != nil || requirement.Contract == nil {
		return err
	}
	// Tags publish their annotated object, whose tested commit is retained on
	// the run. Branch/merge publication must publish precisely the tested commit.
	if publication.RunID != "" && ((run.Release && publication.ResultSHA != run.SHA) || (!run.Release && publication.ResultSHA != candidateSHA)) {
		return fmt.Errorf("publication differs from trusted tested candidate")
	}
	if publication.RunID == "" {
		if sourceErr != nil || run.Status != model.RunPassed || run.TestedSHA != candidateSHA {
			return fmt.Errorf("trusted fast-forward requires a passed exact candidate run")
		}
	}
	return scheduler.trustedReceipt(ctx, repository, run, requirement)
}

func (scheduler *Scheduler) recoverTrustedPreflights(ctx context.Context) error {
	journal, ok := scheduler.store.(vmrunner.KVMJournal)
	if !ok {
		return nil
	}
	pending, err := retryDatabase(ctx, "PendingKVMPreflights", journal.PendingKVMPreflights)
	if err != nil {
		log.Printf("WARN: trusted preflight recovery inventory unavailable: %v", err)
		return nil
	}
	if len(pending) == 0 {
		return nil
	}
	if scheduler.trusted == nil {
		return fmt.Errorf("pending trusted preflight cleanup requires configured executor")
	}
	runs, err := scheduler.trusted.Recover(ctx)
	if err != nil {
		log.Printf("WARN: trusted preflight recovery remains pending: %v", err)
	}
	// Even a partial recovery must interrupt successfully cleaned parents. Other
	// rows retain their journal/capacity obligation and cannot publish.
	for _, id := range runs {
		if err := scheduler.interruptRecoveredPreflight(ctx, id); err != nil {
			log.Printf("WARN: recovered trusted preflight parent %s remains pending: %v", id, err)
		}
	}
	return nil
}

// Execute has already returned. Recover only that run's uncertain preflight;
// the startup-wide Recover inventory could cancel unrelated live executions.
func (scheduler *Scheduler) recoverDatabasePreflight(ctx context.Context, id string, reservation *admissionReservation) error {
	recoverer, ok := scheduler.trusted.(interface {
		RecoverRun(context.Context, string) error
	})
	if !ok {
		return fmt.Errorf("trusted preflight per-run recovery is unavailable")
	}
	total := scheduler.databaseRetryCount.Add(1)
	log.Printf("WARN: scheduler database operation=TrustedPreflight retry_pending=true retries_total=%d", total)
	// This is a composite Kubernetes cleanup, not a DB statement. It must
	// retain its own recovery budget and must not hold a worker forever.
	recoveryCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	err := scheduler.RetryRecovery(recoveryCtx, "RecoverTrustedRun", func(ctx context.Context) error {
		return recoverer.RecoverRun(ctx, id)
	})
	if err != nil {
		pending := recoverablePreflightError(err) || (ctx.Err() != nil && transientDatabaseAttemptError(err, true))
		if !pending {
			return err
		}
		log.Printf("WARN: trusted preflight recovery remains pending run=%s: %v", id, err)
		// The durable journal and retained grant still forbid publication/reuse.
		// Startup recovery can resume this exact obligation after restart.
		return nil
	}
	if err := scheduler.interruptRecoveredPreflight(ctx, id); err != nil {
		return err
	}
	reservation.release()
	return nil
}

func (scheduler *Scheduler) interruptRecoveredPreflight(ctx context.Context, id string) error {
	recoverer, ok := scheduler.store.(interface {
		RecoverKVMPreflight(context.Context, string) (model.Run, error)
	})
	if !ok {
		return fmt.Errorf("trusted preflight durable recovery is unavailable")
	}
	next, err := retryDatabase(ctx, "RecoverKVMPreflight", func(ctx context.Context) (model.Run, error) { return recoverer.RecoverKVMPreflight(ctx, id) })
	if err != nil {
		return err
	}
	scheduler.signals.NotifyRun(id)
	if promotion, err := scheduler.database().PromotionByRun(ctx, id); err == nil {
		scheduler.signals.NotifyPromotion(promotion.ID)
	}
	if next.Trigger == "promotion" {
		if promotion, err := scheduler.database().PromotionByRun(ctx, next.ID); err == nil {
			scheduler.signals.NotifyPromotion(promotion.ID)
		}
	}
	if next.ID != "" {
		scheduler.signals.NotifyRun(next.ID)
		scheduler.signalQueue()
	}
	return nil
}
