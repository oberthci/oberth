package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/protectedtests"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/trustedharness"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

type trustedController interface {
	TrustedKVMWorkflowUID(context.Context, argojob.Request) (string, error)
	TrustedKVMReceipt(context.Context, argojob.Request, string) (vmrunner.KVMReceipt, error)
	CleanupTrustedKVM(context.Context, string, string, string) error
}
type placedPreflightJournal interface {
	ReservePlacedGenericPreflight(context.Context, protectedtests.GenericAdmission, protectedtests.PlacementAdmission) (protectedtests.Execution, error)
}

type genericPreflightJournal interface {
	ReserveGenericPreflight(context.Context, protectedtests.GenericAdmission) (protectedtests.Execution, error)
	GenericAdmission(context.Context, string) (*protectedtests.GenericAdmission, error)
	CheckGenericPreflightAuthority(context.Context, string) error
}

type trustedRunStore interface {
	vmrunner.KVMJournal
	Run(context.Context, string) (model.Run, error)
	Repository(context.Context, int64) (model.Repository, error)
	Upstream(context.Context, int64) (model.Upstream, error)
}

// TrustedPreflights is the only adapter permitted to submit a server-owned KVM
// leaf. Recipes are immutable operator policy, not repository configuration.
type TrustedPreflights struct {
	generic         bool
	jobs            *ArgoJobs
	controller      trustedController
	store           trustedRunStore
	recipes         []vmrunner.KVMRecipe
	workspaceRoot   string
	requireMutation func(context.Context) error
	fetch           func(context.Context, string, string, int64) ([]byte, error)
}

func NewTrustedPreflights(jobs *ArgoJobs, journal trustedRunStore, recipes []vmrunner.KVMRecipe, workspaceRoot string, requireMutation func(context.Context) error) (*TrustedPreflights, error) {
	return newProtectedPreflights(jobs, journal, recipes, workspaceRoot, requireMutation, false)
}

// NewGenericProtectedPreflights is an explicit opt-in. Existing resource grants
// never switch the protected executor by themselves. A missing current group
// permits recovery of persisted obligations only; Supports and Execute refuse.
func NewGenericProtectedPreflights(jobs *ArgoJobs, journal trustedRunStore, recipes []vmrunner.KVMRecipe, workspaceRoot string, requireMutation func(context.Context) error) (*TrustedPreflights, error) {
	return newProtectedPreflights(jobs, journal, recipes, workspaceRoot, requireMutation, true)
}
func newProtectedPreflights(jobs *ArgoJobs, journal trustedRunStore, recipes []vmrunner.KVMRecipe, workspaceRoot string, requireMutation func(context.Context) error, generic bool) (*TrustedPreflights, error) {
	if requireMutation == nil || jobs == nil || journal == nil || (!generic && !jobs.config.ExecutionPlacement.Protected() && (!jobs.config.KVMEnabled || jobs.config.TrustedKVMGroup <= 0)) || len(recipes) == 0 || len(recipes) > 128 {
		return nil, errors.New("trusted preflights require KVM, explicit positive KVM group, journal and approved recipes")
	}
	if generic {
		if jobs.config.TrustedKVMGroup < 0 || jobs.config.TrustedKVMGroup > 2147483647 {
			return nil, errors.New("generic protected group must be zero for recovery or a valid positive GID")
		}
		if jobs.capabilityGrants == nil {
			return nil, errors.New("generic protected executor requires current operator capability loading")
		}
		if _, ok := journal.(genericPreflightJournal); !ok {
			return nil, errors.New("generic protected executor requires durable runtime admission journal")
		}
	}
	controller, ok := jobs.controller.(trustedController)
	if !ok {
		return nil, errors.New("trusted controller unavailable")
	}
	if err := protectedtests.ValidateRecipes(recipes); err != nil {
		return nil, err
	}
	executor := &TrustedPreflights{generic: generic, jobs: jobs, controller: controller, store: journal, recipes: append([]vmrunner.KVMRecipe(nil), recipes...), workspaceRoot: workspaceRoot, requireMutation: requireMutation, fetch: trustedharness.Fetch}
	if native, ok := jobs.controller.(*argojob.Controller); ok {
		native.WithProtectedPlacementAuthority(executor.verifyPlacedAuthority)
	}
	return executor, nil
}
func (p *TrustedPreflights) recipe(contract vmrunner.Contract) (vmrunner.KVMRecipe, error) {
	descriptor, err := protectedtests.SelectDescriptor(contract, p.recipes)
	return descriptor.Recipe(), err
}
func (p *TrustedPreflights) Supports(contract vmrunner.Contract) error {
	if p.jobs != nil && p.jobs.config.ExecutionPlacement.Protected() && !p.generic {
		return errors.New("protected placement generic runtime is not enabled")
	}
	if p.generic && p.jobs.config.TrustedKVMGroup <= 0 {
		return errors.New("generic protected execution requires an explicit positive KVM group")
	}
	_, err := p.recipe(contract)
	return err
}

func (p *TrustedPreflights) request(job service.WorkflowRequest, intent vmrunner.KVMIntent, generic *protectedtests.GenericAdmission) (argojob.Request, error) {
	source, err := readArgoSource(job.SourceDir, pipeline.TriggerCI)
	if err != nil {
		return argojob.Request{}, err
	}
	sum := sha256.Sum256(source)
	if hex.EncodeToString(sum[:]) != intent.WorkflowSpecDigest {
		return argojob.Request{}, errors.New("preflight build blueprint changed")
	}
	kind, err := runKind(job.Run)
	if err != nil {
		return argojob.Request{}, err
	}
	request := argojob.Request{RunID: job.Run.ID, RepositoryID: job.Repository.ID, Name: intent.WorkflowName, Repo: job.Repository.Name, UpstreamName: job.UpstreamName, UpstreamOrg: job.UpstreamOrg, Ref: job.Run.Ref, SHA: intent.CandidateSHA, Trigger: pipeline.TriggerCI, RunTrigger: kind, ScheduleEntry: job.Run.ScheduleEntry, Source: source, SourceDir: job.SourceDir, PipelineFile: ".oberth/build.yaml", ApprovedSecrets: map[string]bool{}, StepGrants: map[string]map[string]bool{}, TrustedKVM: &intent}
	if generic != nil {
		copied, err := protectedtests.NewGenericAdmission(generic.Intent, &generic.Capability, generic.SupplementalGroup)
		if err != nil {
			return argojob.Request{}, err
		}
		request.GenericProtected = &copied
		request.Capability = &copied.Capability
	}
	return request, nil
}

func (p *TrustedPreflights) Execute(ctx context.Context, job service.WorkflowRequest, requirement vmrunner.Requirement, output io.Writer) (retErr error) {
	if err := p.jobs.checkProtectedTarget(ctx, job); err != nil {
		return err
	}
	if p.jobs != nil && p.jobs.config.ExecutionPlacement.Protected() && !p.generic {
		return errors.New("protected placement generic runtime is not enabled")
	}
	if p.generic && p.jobs.config.TrustedKVMGroup <= 0 {
		return errors.New("generic protected execution requires an explicit positive KVM group")
	}
	if requirement.Contract == nil {
		return errors.New("missing trusted contract")
	}
	descriptor, err := protectedtests.SelectDescriptor(*requirement.Contract, p.recipes)
	if err != nil {
		return err
	}
	recipe := descriptor.Recipe()
	var capability *argoworkflow.Capability
	if p.generic {
		capability, err = p.jobs.capabilityGrants(ctx, job.Repository.ID)
		if err != nil {
			return err
		}
		if capability == nil || capability.Validate() != nil || capability.Namespace != p.jobs.config.Namespace || capability.ExtendedResources[protectedtests.GenericDeviceResource] < 1 {
			return errors.New("protected generic execution requires current device authority in its exact namespace")
		}
	}

	source, err := readArgoSource(job.SourceDir, pipeline.TriggerCI)
	if err != nil {
		return err
	}
	if err := p.checkPlacedPreparation(job, requirement, recipe, capability, source); err != nil {
		return err
	}
	blueprint, err := argoworkflow.Decode(source)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	duration := p.jobs.config.ExecutionTimeout(model.CanonicalRepoKey(job.UpstreamName, job.UpstreamOrg, job.Repository.Name), blueprint.Spec.ActiveDeadlineSeconds)
	if duration > vmrunner.MaxDeadline {
		duration = vmrunner.MaxDeadline
	}
	deadline, err := p.jobs.config.ExecutionPlacement.ProtectedDeadline(now, now.Add(duration))
	if err != nil {
		return err
	}
	duration = deadline.Sub(now)
	parent := ctx
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	defer func() {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && parent.Err() == nil {
			retErr = errors.Join(service.ErrPreflightDeadline, retErr)
		}
	}()
	// Approval is checked BEFORE fetching/executing any suite-selected content.
	bundle, err := p.fetch(ctx, recipe.SuiteURL, recipe.SuiteSHA256, vmrunner.MaxSuiteBundleBytes)
	if err != nil {
		return err
	}
	suite, err := descriptor.VerifyBundle(bundle)
	if err != nil {
		return err
	}
	inventory, count := suite.Inventory()
	sum := sha256.Sum256(source)
	if job.UpstreamOrg == "" {
		job.UpstreamOrg = job.UpstreamName
	}
	intent := vmrunner.KVMIntent{RunID: job.Run.ID, RepoID: job.Repository.ID, Repository: model.CanonicalRepoKey(job.UpstreamName, job.UpstreamOrg, job.Repository.Name), CandidateSHA: requirement.CandidateSHA, BaselineSHA: requirement.BaselineSHA, Contract: descriptor.Contract(), Recipe: recipe, Namespace: p.jobs.controller.Namespace(), WorkflowName: job.WorkflowName + "-trusted", WorkflowSpecDigest: hex.EncodeToString(sum[:]), InventoryDigest: inventory, CaseCount: count, AdmittedAt: now, Deadline: now.Add(duration)}
	var generic *protectedtests.GenericAdmission
	if p.generic {
		admission, err := protectedtests.NewGenericAdmission(intent, capability, p.jobs.config.TrustedKVMGroup)
		if err != nil {
			return err
		}
		generic = &admission
	}
	submission, err := p.request(job, intent, generic)
	if err != nil {
		return err
	}
	if !p.generic && p.jobs.kubeVirtGrants != nil {
		budget, granted, loadErr := p.jobs.kubeVirtGrants.ActiveKubeVirtGrant(ctx, job.Repository.ID)
		if loadErr != nil {
			return fmt.Errorf("preflight load VM grant: %w", loadErr)
		}
		submission.KubeVirtGranted = granted
		if granted {
			submission.KubeVirtBudget = kubeVirtBudget(budget)
		}
	}
	if err := p.jobs.bindResourceMaximum(ctx, &submission); err != nil {
		return err
	}
	if preparer, ok := p.jobs.controller.(interface {
		PrepareNonroot(context.Context, argojob.Request) (argojob.Request, error)
	}); ok {
		submission, err = preparer.PrepareNonroot(ctx, submission)
		if err != nil {
			return err
		}
	}
	// Build validates closed credentials and the appended template before any
	// capacity reservation or external create. This is the same source sealed in
	// intent and subsequently seeded to a fresh branch-tier claim.
	if _, err = argojob.Build(p.jobs.config, submission); err != nil {
		return err
	}
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return err
	}
	if _, declared := workflow.Annotations[argoworkflow.KVMTemplatesAnnotation]; declared {
		if err := p.jobs.auditSubmission(ctx, job, submission); err != nil {
			return err
		}
	}
	if err = p.requireMutation(ctx); err != nil {
		return err
	}
	placed, err := argojob.ProtectedAdmission(p.jobs.config, submission)
	if err != nil {
		return err
	}
	var execution protectedtests.Execution
	if placed != nil {
		journal, ok := p.store.(placedPreflightJournal)
		if !ok || generic == nil {
			return errors.New("protected placement journal unavailable")
		}
		execution, err = journal.ReservePlacedGenericPreflight(ctx, *generic, *placed)
	} else if generic != nil {
		execution, err = p.store.(genericPreflightJournal).ReserveGenericPreflight(ctx, *generic)
	} else {
		execution, err = p.store.ReserveKVMPreflight(ctx, intent)
	}
	if err != nil {
		return err
	}
	if execution.Submitted || execution.Cleaned {
		return errors.New("trusted preflight already has a lifecycle; retry with a fresh run")
	}
	watched := make(chan struct{})
	defer func() { cancel(); <-watched }()
	go func() {
		defer close(watched)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		p.watchParentRun(ctx, intent.RunID, ticker.C, cancel)
	}()
	uid := ""
	submitted := false
	rejected := false
	refusalCode := "preflight_failed"
	defer func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if retErr != nil {
			state, stateErr := p.store.KVMPreflight(cleanupCtx, intent.RunID)
			if stateErr != nil {
				retErr = errors.Join(retErr, stateErr)
				return
			}
			if state.Receipt == nil && state.Refusal == nil {
				code := refusalCode
				if rejected {
					code = "create_refused"
				} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					code = "deadline"
				} else if ctx.Err() != nil {
					code = "canceled"
				}
				refusal := vmrunner.KVMRefusal{Code: code, ObservedAt: time.Now().UTC()}
				if observer, ok := p.controller.(interface {
					TrustedKVMRefusal(context.Context, argojob.Request, string) (*vmrunner.KVMTerminationRecord, error)
				}); ok && uid != "" {
					if observed, e := observer.TrustedKVMRefusal(cleanupCtx, submission, uid); e == nil {
						refusal.Termination = observed
					}
				}
				if e := p.store.RecordKVMRefusal(cleanupCtx, intent.RunID, refusal); e != nil {
					retErr = errors.Join(retErr, e)
					return
				}
			}
		}
		if submitted && !rejected && uid == "" {
			var lookupErr error
			uid, lookupErr = p.controller.TrustedKVMWorkflowUID(cleanupCtx, submission)
			if lookupErr != nil && !trustedMismatchCleanupUID(uid, lookupErr) {
				retErr = errors.Join(retErr, fmt.Errorf("uncertain trusted create retains capacity: %w", lookupErr))
				return
			}

		}
		if uid != "" {
			if e := p.store.BindKVMPreflight(cleanupCtx, intent.RunID, uid); e != nil {
				retErr = errors.Join(retErr, e)
				return
			}
			if e := p.controller.CleanupTrustedKVM(cleanupCtx, intent.WorkflowName, intent.RunID, uid); e != nil {
				retErr = errors.Join(retErr, e)
				return
			}
		}
		retErr = errors.Join(retErr, p.store.CompleteKVMPreflightCleanup(cleanupCtx, intent.RunID, uid))
	}()
	if err = p.requireMutation(ctx); err != nil {
		return err
	}
	if err = p.store.SubmitKVMPreflight(ctx, intent.RunID); err != nil {
		return err
	}
	submitted = true
	if _, err = p.jobs.controller.Create(ctx, submission); err != nil {
		if errors.Is(err, argojob.ErrTrustedKVMCreateRefused) {
			rejectCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
			rejectErr := p.store.RejectKVMPreflight(rejectCtx, intent.RunID)
			stop()
			if rejectErr != nil {
				return errors.Join(err, rejectErr)
			}
			rejected = true
		}
		return err
	}
	uid, err = p.controller.TrustedKVMWorkflowUID(ctx, submission)
	if err != nil {
		return err
	}
	if err = p.store.BindKVMPreflight(ctx, intent.RunID, uid); err != nil {
		return err
	}
	completion, err := p.jobs.controller.Wait(ctx, intent.WorkflowName, intent.RunID, output)
	if err != nil {
		return p.failure(completion, err)
	}
	if !completion.Succeeded {
		return p.failure(completion, errors.New(p.jobs.result(completion).Error))
	}
	refusalCode = "observation_refused"
	receipt, err := p.controller.TrustedKVMReceipt(ctx, submission, uid)
	if err != nil {
		return p.failure(completion, err)
	}
	if err = receipt.Validate(intent); err != nil {
		return p.failure(completion, err)
	}
	if err = p.store.RecordKVMReceipt(ctx, intent.RunID, receipt); err != nil {
		return p.failure(completion, err)
	}
	return nil
}

func (p *TrustedPreflights) failure(completion argojob.Completion, cause error) error {
	result := p.jobs.result(completion)
	if result.Error != "" && result.Error != cause.Error() {
		cause = fmt.Errorf("%s: %w", result.Error, cause)
	}
	// A command failure has an observed failed step. Missing terminal evidence,
	// a failed Workflow/Pod, and receipt collection failures remain infrastructure.
	if completion.Succeeded || completion.Infrastructure || result.FailedStep == "" {
		result.Phase = "infrastructure"
		result.FailedStage, result.FailedStep = "", ""
	}
	result.Status = model.RunFailed
	result.Error = cause.Error()
	return &service.TrustedPreflightFailure{Result: result, Cause: fmt.Errorf("trusted preflight: %w", cause)}
}

// trustedRecoveryBound caps one execution's whole startup recovery: the
// absence observation, identity reconciliation, negative refusal and cleanup.
const trustedRecoveryBound = 2 * time.Minute

// Recover never replays execution or grants publication. It cleans durable
// obligations first; the scheduler then interrupts undelivered parents and
// may durably queue one fresh attempt.
// A completed transient read is not evidence the parent was canceled. Keep
// observing under the execution deadline; only authoritative state or a hard
// read failure cancels the credential-free preflight.
func (p *TrustedPreflights) watchParentRun(ctx context.Context, id string, ticks <-chan time.Time, cancel context.CancelFunc) {
	var retries uint64
	var nextLog time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			run, err := p.store.Run(readCtx, id)
			stop()
			if service.IsTransientDatabaseError(err) && ctx.Err() == nil {
				retries++
				if now := time.Now(); !now.Before(nextLog) {
					log.Printf("WARN: trusted parent DB read run=%s retry_pending=true retries_total=%d error=%q", id, retries, err)
					nextLog = now.Add(30 * time.Second)
				}
				continue
			}
			if err != nil || run.Status != model.RunRunning || run.SupersededBy != "" {
				cancel()
				return
			}
		}
	}
}

func (p *TrustedPreflights) Recover(ctx context.Context) ([]string, error) {
	pending, err := p.store.PendingKVMPreflights(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	var failures []error
	for _, execution := range pending {
		// A stuck deletion or ambiguous create cannot hold startup forever.
		recoveryCtx, cancel := context.WithTimeout(ctx, trustedRecoveryBound)
		recoveryErr := p.recoverOne(recoveryCtx, execution)
		cancel()
		if err := recoveryErr; err != nil {
			log.Printf("WARN: trusted preflight recovery pending for run %s: %v", execution.Intent.RunID, err)
			failures = append(failures, err)
			continue
		}
		ids = append(ids, execution.Intent.RunID)
	}
	return ids, errors.Join(failures...)
}

// RecoverRun is used after the live owner of this exact execution has returned
// a transient storage failure. Never enumerate/cancel another live preflight.
func (p *TrustedPreflights) RecoverRun(ctx context.Context, id string) error {
	execution, err := p.store.KVMPreflight(ctx, id)
	if err != nil {
		return err
	}
	return p.recoverOne(ctx, execution)
}

func (p *TrustedPreflights) recoverOne(ctx context.Context, execution vmrunner.KVMExecution) error {
	intent := execution.Intent
	controller := p.controller
	if native, ok := controller.(*argojob.Controller); ok {
		recovered, err := native.ProtectedRecoveryController(ctx, intent.RunID)
		if err != nil {
			return err
		}
		controller = recovered
	}
	uid := execution.WorkflowUID
	if execution.Cleaned {
		return nil
	}
	if execution.Submitted && !execution.Rejected && uid == "" {
		observer, ok := controller.(interface {
			ConfirmTrustedKVMAbsent(context.Context, string) error
		})
		if !ok {
			return errors.New("trusted absence observer unavailable")
		}
		// This path intentionally precedes checkout/repository reads: the process
		// may have died before Create, and its workspace may no longer exist.
		absenceErr := observer.ConfirmTrustedKVMAbsent(ctx, intent.WorkflowName)
		if absenceErr == nil {
			if err := p.store.RejectKVMPreflight(ctx, intent.RunID); err != nil {
				return err
			}
		} else {
			if !errors.Is(absenceErr, argojob.ErrTrustedKVMWorkflowPresent) {
				return absenceErr
			}
			run, err := p.store.Run(ctx, intent.RunID)
			if err != nil {
				return err
			}
			repo, err := p.store.Repository(ctx, intent.RepoID)
			if err != nil {
				return err
			}
			upstream, err := p.store.Upstream(ctx, repo.UpstreamID)
			if err != nil {
				return err
			}
			job := service.WorkflowRequest{Run: run, Repository: repo, UpstreamName: upstream.Name, UpstreamOrg: upstream.Org(), SourceDir: filepath.Join(p.workspaceRoot, run.ID, "src")}
			var generic *protectedtests.GenericAdmission
			if journal, ok := p.store.(genericPreflightJournal); ok {
				generic, err = journal.GenericAdmission(ctx, intent.RunID)
				if err != nil {
					return err
				}
			}
			request, err := p.request(job, intent, generic)
			if err != nil {
				return err
			}
			// Reconstruct the already submitted spec for observation and cleanup
			// only. A revoked grant must not strand its VM capacity obligation.
			// This path never creates a workflow; the observer requires an exact
			// match of the journal-bound intent and full emitted specification.
			// The emitted spec does not depend on the grant budget, so admission
			// is checked against the largest budget any grant may name rather
			// than the default: a live grant above the default (or one lowered
			// or revoked since submission) must not strand reconciliation.
			if generic == nil {
				request.KubeVirtGranted = true
				request.KubeVirtBudget = argoworkflow.VMIBudgetLimit()
			}
			uid, err = controller.TrustedKVMWorkflowUID(ctx, request)
			if err != nil && !trustedMismatchCleanupUID(uid, err) {
				return fmt.Errorf("uncertain trusted create still requires reconciliation: %w", err)
			}
			if err := p.store.BindKVMPreflight(ctx, intent.RunID, uid); err != nil {
				return err
			}
		}
	}
	if execution.Receipt == nil && execution.Refusal == nil {
		if err := p.store.RecordKVMRefusal(ctx, intent.RunID, vmrunner.KVMRefusal{Code: "canceled", ObservedAt: time.Now().UTC()}); err != nil {
			return err
		}
	}
	if uid != "" {
		if err := controller.CleanupTrustedKVM(ctx, intent.WorkflowName, intent.RunID, uid); err != nil {
			return err
		}
	}
	return p.store.CompleteKVMPreflightCleanup(ctx, intent.RunID, uid)
}

var _ service.TrustedPreflight = (*TrustedPreflights)(nil)

func (p *TrustedPreflights) RecipeAllowed(recipe vmrunner.KVMRecipe) error {
	return protectedtests.RecipeAllowed(recipe, p.recipes)
}
func (p *TrustedPreflights) Size(_ context.Context, job service.WorkflowRequest) (pipeline.Size, error) {
	source, err := readArgoSource(job.SourceDir, pipeline.TriggerCI)
	if err != nil {
		return "", err
	}
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return "", err
	}
	// The preflight runs build.yaml. For a tag run that document has no tag
	// entry, and the preflight gates a release, so it is sized as the
	// full-gate promotion run of the same document.
	trigger, err := runKind(job.Run)
	if err != nil {
		return "", err
	}
	if trigger == argoworkflow.RunTriggerTag {
		trigger = argoworkflow.RunTriggerPromotion
	}
	size, err := argoworkflow.DeclaredSizeFor(workflow, trigger)
	if err != nil {
		return "", err
	}
	// The server leaf itself needs two CPUs/four GiB even for an S build.
	if size == pipeline.S {
		size = pipeline.M
	}
	return size, nil
}

// An observer may bind an owned, altered Workflow solely for UID-pinned cleanup.
// A foreign run/source never supplies a cleanup UID with this classification.
func trustedMismatchCleanupUID(uid string, err error) bool {
	var mismatch interface{ WorkflowSpecMismatch() bool }
	return uid != "" && errors.As(err, &mismatch) && mismatch.WorkflowSpecMismatch()
}

// CancelPreflight requests deletion without waiting for Pod finalizers. The
// journal retains VM capacity and the absence-readback obligation independently.
func (p *TrustedPreflights) CancelPreflight(ctx context.Context, id string) error {
	execution, err := p.store.KVMPreflight(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil || execution.Cleaned || !execution.Submitted || execution.Rejected {
		return err
	}
	controller, ok := p.controller.(interface {
		CancelTrustedKVM(context.Context, vmrunner.KVMIntent, string) error
	})
	if !ok {
		return errors.New("trusted cancellation controller unavailable")
	}
	return controller.CancelTrustedKVM(ctx, execution.Intent, execution.WorkflowUID)
}
