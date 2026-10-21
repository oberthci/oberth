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
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/trustedharness"
	"github.com/oberthci/oberth/internal/vmrunner"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type trustedController interface {
	TrustedKVMWorkflowUID(context.Context, argojob.Request) (string, error)
	TrustedKVMReceipt(context.Context, argojob.Request, string) (vmrunner.KVMReceipt, error)
	CleanupTrustedKVM(context.Context, string, string, string) error
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
	jobs            *ArgoJobs
	controller      trustedController
	store           trustedRunStore
	recipes         []vmrunner.KVMRecipe
	workspaceRoot   string
	requireMutation func(context.Context) error
	fetch           func(context.Context, string, string, int64) ([]byte, error)
}

func NewTrustedPreflights(jobs *ArgoJobs, journal trustedRunStore, recipes []vmrunner.KVMRecipe, workspaceRoot string, requireMutation func(context.Context) error) (*TrustedPreflights, error) {
	if requireMutation == nil || jobs == nil || journal == nil || !jobs.config.KVMEnabled || jobs.config.TrustedKVMGroup <= 0 || len(recipes) == 0 || len(recipes) > 128 {
		return nil, errors.New("trusted preflights require KVM, explicit positive KVM group, journal and approved recipes")
	}
	controller, ok := jobs.controller.(trustedController)
	if !ok {
		return nil, errors.New("trusted controller unavailable")
	}
	seen := map[string]bool{}
	for _, r := range recipes {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		key := r.SuiteSHA256 + ":" + r.GuestSHA256
		if seen[key] {
			return nil, errors.New("duplicate trusted recipe")
		}
		seen[key] = true
	}
	return &TrustedPreflights{jobs: jobs, controller: controller, store: journal, recipes: append([]vmrunner.KVMRecipe(nil), recipes...), workspaceRoot: workspaceRoot, requireMutation: requireMutation, fetch: trustedharness.Fetch}, nil
}
func (p *TrustedPreflights) recipe(contract vmrunner.Contract) (vmrunner.KVMRecipe, error) {
	if vmrunner.ValidateContract(contract) != nil || contract.Version != 2 {
		return vmrunner.KVMRecipe{}, errors.New("protected suite executor requires tests.yaml v2")
	}
	for _, r := range p.recipes {
		if r.SuiteSHA256 == contract.SuiteSHA256 && r.GuestSHA256 == contract.GuestSHA256 {
			return r, nil
		}
	}
	return vmrunner.KVMRecipe{}, errors.New("suite and guest recipe is not operator-approved")
}
func (p *TrustedPreflights) Supports(contract vmrunner.Contract) error {
	_, err := p.recipe(contract)
	return err
}

func (p *TrustedPreflights) request(job service.JobRequest, intent vmrunner.KVMIntent) (argojob.Request, error) {
	source, err := readArgoSource(job.SourceDir, periapsis.TriggerCI)
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
	return argojob.Request{RunID: job.Run.ID, Name: intent.WorkflowName, Repo: job.Repository.Name, UpstreamName: job.UpstreamName, UpstreamOrg: job.UpstreamOrg, Ref: job.Run.Ref, SHA: intent.CandidateSHA, Trigger: periapsis.TriggerCI, RunTrigger: kind, Source: source, SourceDir: job.SourceDir, ApprovedSecrets: map[string]bool{}, StepGrants: map[string]map[string]bool{}, TrustedKVM: &intent}, nil
}

func (p *TrustedPreflights) Execute(ctx context.Context, job service.JobRequest, requirement vmrunner.Requirement, output io.Writer) (retErr error) {
	if requirement.Contract == nil {
		return errors.New("missing trusted contract")
	}
	recipe, err := p.recipe(*requirement.Contract)
	if err != nil {
		return err
	}
	// Approval is checked BEFORE fetching/executing any suite-selected content.
	bundle, err := p.fetch(ctx, recipe.SuiteURL, recipe.SuiteSHA256, vmrunner.MaxSuiteBundleBytes)
	if err != nil {
		return err
	}
	suite, err := vmrunner.VerifyKVMSuiteBundle(*requirement.Contract, bundle)
	if err != nil {
		return err
	}
	inventory, count := suite.Inventory()
	source, err := readArgoSource(job.SourceDir, periapsis.TriggerCI)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(source)
	if job.UpstreamOrg == "" {
		job.UpstreamOrg = job.UpstreamName
	}
	now := time.Now().UTC()
	duration := p.jobs.config.WorkflowTimeout
	if duration <= 0 || duration > vmrunner.MaxDeadline {
		duration = vmrunner.MaxDeadline
	}
	intent := vmrunner.KVMIntent{RunID: job.Run.ID, RepoID: job.Repository.ID, Repository: strings.Join([]string{job.UpstreamName, job.UpstreamOrg, job.Repository.Name}, "/"), CandidateSHA: requirement.CandidateSHA, BaselineSHA: requirement.BaselineSHA, Contract: *requirement.Contract, Recipe: recipe, Namespace: p.jobs.controller.Namespace(), WorkflowName: job.JobName + "-trusted", WorkflowSpecDigest: hex.EncodeToString(sum[:]), InventoryDigest: inventory, CaseCount: count, AdmittedAt: now, Deadline: now.Add(duration)}
	submission, err := p.request(job, intent)
	if err != nil {
		return err
	}
	if p.jobs.kubeVirtGrants != nil {
		budget, granted, loadErr := p.jobs.kubeVirtGrants.ActiveKubeVirtGrant(ctx, job.Repository.ID)
		if loadErr != nil {
			return fmt.Errorf("preflight load VM grant: %w", loadErr)
		}
		submission.KubeVirtGranted = granted
		if granted {
			submission.KubeVirtBudget = kubeVirtBudget(budget)
		}
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
	execution, err := p.store.ReserveKVMPreflight(ctx, intent)
	if err != nil {
		return err
	}
	if execution.Submitted || execution.Cleaned {
		return errors.New("trusted preflight already has a lifecycle; retry with a fresh run")
	}
	ctx, cancel := context.WithDeadline(ctx, intent.Deadline)
	defer cancel()
	watched := make(chan struct{})
	defer func() { cancel(); <-watched }()
	go func() {
		defer close(watched)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run, e := p.store.Run(ctx, intent.RunID)
				if e != nil || run.Status != model.RunRunning || run.SupersededBy != "" {
					cancel()
					return
				}
			}
		}
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
			if lookupErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("uncertain trusted create retains capacity: %w", lookupErr))
				return
			}
			if e := p.store.BindKVMPreflight(cleanupCtx, intent.RunID, uid); e != nil {
				retErr = errors.Join(retErr, e)
				return
			}
		}
		if uid != "" {
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
		return err
	}
	if !completion.Succeeded {
		return errors.New("trusted preflight Workflow failed")
	}
	refusalCode = "observation_refused"
	receipt, err := p.controller.TrustedKVMReceipt(ctx, submission, uid)
	if err != nil {
		return err
	}
	if err = receipt.Validate(intent); err != nil {
		return err
	}
	return p.store.RecordKVMReceipt(ctx, intent.RunID, receipt)
}

// Recover never replays execution or grants publication. It cleans durable
// obligations first; the scheduler then interrupts parents that have no Job.
func (p *TrustedPreflights) Recover(ctx context.Context) ([]string, error) {
	pending, err := p.store.PendingKVMPreflights(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	var failures []error
	for _, execution := range pending {
		if err := p.recoverOne(ctx, execution); err != nil {
			log.Printf("WARN: trusted preflight recovery pending for run %s: %v", execution.Intent.RunID, err)
			failures = append(failures, err)
			continue
		}
		ids = append(ids, execution.Intent.RunID)
	}
	return ids, errors.Join(failures...)
}

func (p *TrustedPreflights) recoverOne(ctx context.Context, execution vmrunner.KVMExecution) error {
	intent := execution.Intent
	uid := execution.WorkflowUID
	if execution.Cleaned {
		return nil
	}
	if execution.Submitted && !execution.Rejected && uid == "" {
		observer, ok := p.controller.(interface {
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
			job := service.JobRequest{Run: run, Repository: repo, UpstreamName: upstream.Name, UpstreamOrg: upstream.Org(), SourceDir: filepath.Join(p.workspaceRoot, run.ID, "src")}
			request, err := p.request(job, intent)
			if err != nil {
				return err
			}
			// Reconstruct the already submitted spec for observation and cleanup
			// only. A revoked grant must not strand its VM capacity obligation.
			// This path never creates a workflow; the observer requires an exact
			// match of the journal-bound intent and full emitted specification.
			request.KubeVirtGranted = true
			uid, err = p.controller.TrustedKVMWorkflowUID(ctx, request)
			if err != nil {
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
		if err := p.controller.CleanupTrustedKVM(ctx, intent.WorkflowName, intent.RunID, uid); err != nil {
			return err
		}
	}
	return p.store.CompleteKVMPreflightCleanup(ctx, intent.RunID, uid)
}

var _ service.TrustedPreflight = (*TrustedPreflights)(nil)

func (p *TrustedPreflights) RecipeAllowed(recipe vmrunner.KVMRecipe) error {
	for _, allowed := range p.recipes {
		if allowed == recipe {
			return nil
		}
	}
	return errors.New("journaled trusted recipe has been revoked or changed")
}
func (p *TrustedPreflights) Size(_ context.Context, job service.JobRequest) (periapsis.Size, error) {
	source, err := readArgoSource(job.SourceDir, periapsis.TriggerCI)
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
	if size == periapsis.S {
		size = periapsis.M
	}
	return size, nil
}
