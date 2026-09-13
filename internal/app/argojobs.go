package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// argoControl is the engine seam, narrowed the same way jobControl narrows the
// Kubernetes Job engine.
type argoControl interface {
	Create(context.Context, argojob.Request) (string, error)
	Wait(context.Context, string, string, io.Writer) (argojob.Completion, error)
	Cancel(context.Context, string, string) error
	TerminalState(context.Context, string) (*argojob.Completion, error)
	Owns(context.Context, string) (bool, error)
	Namespace() string
}

// SecretAccessLoader loads approved secret grants for a repository at
// admission time. The store.Store satisfies this interface directly.
// The repository is identified by its durable ID to prevent same-name
// repos under different upstreams from aliasing each other's grants.
type SecretAccessLoader interface {
	ActiveSecretGrants(ctx context.Context, repoID int64) (map[string]map[string]bool, error)
}

// ReconcilerHealthChecker reports whether the access reconciler has completed
// at least one successful reconciliation. Credentialed admission must not
// proceed before the initial reconcile succeeds, because a transient initial
// ConfigMap Get failure would leave stale grants active in sqlite.
type ReconcilerHealthChecker interface {
	ReconcileHealthy() bool
}

// ArgoJobs adapts the Argo execution engine to the same control-plane
// contracts the Kubernetes Job engine satisfies, so the scheduler, admission
// gate, store, and audit chain need no knowledge of which engine ran a run.
type ArgoJobs struct {
	controller        argoControl
	auditor           service.Auditor
	config            argojob.Config
	secretAccess      SecretAccessLoader
	fragments         FragmentLoader
	collector         ArtifactCollector
	artifacts         ArtifactStore
	artifactLimit     int64
	artifactBudget    int64
	artifactFailures  map[string]string
	reconcilerHealthy ReconcilerHealthChecker

	mu      sync.Mutex
	intents map[string]argoIntent
}

// argoIntent tracks the lifecycle of one in-flight Workflow submission.
//
// State machine (all transitions under jobs.mu):
//
//	reserved  ──controller.Create()──▸  created=true
//	    │                                   │
//	    ▼                                   ▼
//	cancelled=true                     cancelled=true
//	(Delete before Create)             (Delete after Create)
//
// Delete arriving before Create marks the intent cancelled and returns
// success. The in-flight create() checks cancelled between reservation and
// seeding (cheap early abort) and again after controller.Create returns. If
// the workflow was created but the intent was cancelled in the meantime,
// create() cancels the live workflow and returns ErrSuperseded.
//
// Wait() is unaffected: it reads the intent for its runID and then removes
// the intent on completion. A cancelled-but-created intent is cleaned up by
// create(), not by Wait(), because create() is the only goroutine that
// transitions from reserved to created.
//
// The failure-cleanup defer in create() must NOT remove an intent that
// Delete has marked cancelled: Delete owns the intent's lifecycle once it
// sets cancelled=true.
type argoIntent struct {
	runID     string
	trigger   periapsis.Trigger
	created   bool // true once controller.Create has returned successfully
	cancelled bool // true once Delete has requested cancellation
}

// ErrSuperseded is returned by create() when Delete() cancelled the intent
// while the Workflow was being created. The scheduler records the run as
// cancelled rather than as a creation failure.
var ErrSuperseded = errors.New("app: Workflow superseded during creation")

// SetReconcilerHealth wires the reconciler health check after construction.
// This exists because the reconciler may be created after the engine in the
// initialization order. Once set, credentialed admission blocks until the
// reconciler reports at least one successful reconciliation.
func (jobs *ArgoJobs) SetReconcilerHealth(checker ReconcilerHealthChecker) {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.reconcilerHealthy = checker
}

func NewArgoJobs(controller argoControl, config argojob.Config, auditor service.Auditor, secretAccess SecretAccessLoader, fragments FragmentLoader) (*ArgoJobs, error) {
	if controller == nil {
		return nil, errors.New("app: Argo Workflow controller is required")
	}
	if auditor == nil {
		// The submission audit action is the only record that joins Oberth's
		// chain to OpenBao's own audit device. Without it a release run's
		// (run, trigger, namespace, ServiceAccount) binding would exist
		// nowhere durable.
		return nil, errors.New("app: the Argo engine requires the audit chain")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &ArgoJobs{controller: controller, auditor: auditor, config: config, secretAccess: secretAccess, fragments: fragments, intents: map[string]argoIntent{}}, nil
}

func (jobs *ArgoJobs) CreateCI(ctx context.Context, request service.JobRequest) error {
	return jobs.create(ctx, request, periapsis.TriggerCI)
}

func (jobs *ArgoJobs) CreateRelease(ctx context.Context, request service.JobRequest) error {
	return jobs.create(ctx, request, periapsis.TriggerRelease)
}

// PipelineSize reads the run's declared scheduler weight from the checked-out
// document without evaluating anything.
func (jobs *ArgoJobs) PipelineSize(_ context.Context, request service.JobRequest) (periapsis.Size, error) {
	trigger, err := runTrigger(request.Run)
	if err != nil {
		return "", err
	}
	source, err := readArgoSource(request.SourceDir, trigger)
	if err != nil {
		return "", noPipelineError(err, trigger, request.Repository.Name)
	}
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return "", fmt.Errorf("app: read repository resource contract: %w", err)
	}
	size, err := argoworkflow.DeclaredSize(workflow)
	if err != nil {
		return "", fmt.Errorf("app: read repository resource contract: %w", err)
	}
	return size, nil
}

// PlannedSteps enumerates the run's declared step list from the checked-out
// document, without evaluating anything and without a second trust surface:
// the same Decode the submission path uses, then a read-only walk of the
// template graph it already validated.
//
// The plan is what makes a run's step list describe the pipeline rather than
// its own history so far. Argo's node tree, which every other step projection
// derives from, gains a node only once the controller creates that node's Pod
// -- so a run that has not reached a burn does not report it as pending, it
// omits it, and an agent reading `status` on a failed run sees a plausible
// complete pipeline that is missing exactly the work that never ran.
func (jobs *ArgoJobs) PlannedSteps(_ context.Context, request service.JobRequest) ([]runprogress.PlannedStep, error) {
	trigger, err := runTrigger(request.Run)
	if err != nil {
		return nil, err
	}
	source, err := readArgoSource(request.SourceDir, trigger)
	if err != nil {
		return nil, noPipelineError(err, trigger, request.Repository.Name)
	}
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return nil, fmt.Errorf("app: read repository pipeline document: %w", err)
	}
	planned, err := argoworkflow.PlannedSteps(workflow)
	if err != nil {
		return nil, fmt.Errorf("app: enumerate pipeline steps: %w", err)
	}
	steps := make([]runprogress.PlannedStep, 0, len(planned))
	for _, step := range planned {
		steps = append(steps, runprogress.PlannedStep{Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal})
	}
	return steps, nil
}

func (jobs *ArgoJobs) create(ctx context.Context, request service.JobRequest, trigger periapsis.Trigger) error {
	if strings.TrimSpace(request.JobName) == "" || strings.TrimSpace(request.Run.ID) == "" || request.Repository.Name == "" {
		return errors.New("app: deterministic Workflow name, run, and repository are required")
	}
	// The durable run's own classification decides the trigger, never the
	// caller and never the document. This is the gate the ServiceAccount
	// switch hangs off.
	if expected, err := runTrigger(request.Run); err != nil || expected != trigger {
		return errors.New("app: durable run trigger does not match the requested Workflow capability")
	}

	// Reserve an in-flight intent under the lock. If another create for the
	// same name is already in flight, coordinate through the reservation
	// rather than serializing the whole pipeline behind it. The lock is
	// held only for the map check and the reconciler health gate (both
	// in-memory), not for audit/seeding/controller calls.
	jobs.mu.Lock()
	if existing, occupied := jobs.intents[request.JobName]; occupied {
		jobs.mu.Unlock()
		if existing.runID == request.Run.ID {
			return nil // same submission already in flight
		}
		return fmt.Errorf("app: Workflow %s is in flight for a different run %s", request.JobName, existing.runID)
	}
	// Gate on reconciler health: a credentialed submission must not proceed
	// when the access reconciler has never successfully read the ConfigMap.
	if jobs.secretAccess != nil && jobs.reconcilerHealthy != nil && !jobs.reconcilerHealthy.ReconcileHealthy() {
		jobs.mu.Unlock()
		return fmt.Errorf("app: access reconciler has not completed initial reconciliation; credentialed admission is blocked for %s", request.Repository.Name)
	}
	jobs.intents[request.JobName] = argoIntent{runID: request.Run.ID, trigger: trigger}
	jobs.mu.Unlock()

	// Everything below runs outside the lock. On failure, remove the
	// reservation so a retry or a different run can claim the name —
	// UNLESS Delete has already marked the intent cancelled, in which case
	// Delete owns the intent lifecycle and this defer must not remove it.
	success := false
	defer func() {
		if !success {
			jobs.mu.Lock()
			if current, ok := jobs.intents[request.JobName]; ok && current.runID == request.Run.ID && !current.cancelled {
				delete(jobs.intents, request.JobName)
			}
			jobs.mu.Unlock()
		}
	}()

	// Early abort: Delete arrived between reservation and seeding.
	jobs.mu.Lock()
	if jobs.intents[request.JobName].cancelled {
		// Delete already marked the intent cancelled and owns cleanup.
		// Remove the tombstone now — no workflow exists to cancel.
		delete(jobs.intents, request.JobName)
		jobs.mu.Unlock()
		return ErrSuperseded
	}
	jobs.mu.Unlock()

	source, err := readArgoSource(request.SourceDir, trigger)
	if err != nil {
		return noPipelineError(err, trigger, request.Repository.Name)
	}
	testedSHA := request.Run.TestedSHA
	if testedSHA == "" {
		testedSHA = request.Run.SHA
	}
	// Pre-load approved secrets from the approval table.
	var approvedSecrets map[string]bool
	if jobs.secretAccess != nil {
		grants, loadErr := jobs.secretAccess.ActiveSecretGrants(ctx, request.Repository.ID)
		if loadErr != nil {
			return fmt.Errorf("app: load approved secrets for %s: %w", request.Repository.Name, loadErr)
		}
		approvedSecrets = make(map[string]bool)
		for _, secrets := range grants {
			for secret := range secrets {
				approvedSecrets[secret] = true
			}
		}
	}
	fragments, err := loadFragments(ctx, jobs.fragments, source)
	if err != nil {
		return fmt.Errorf("app: resolve fragments for %s: %w", request.Repository.Name, err)
	}
	submission := argojob.Request{
		RunID: request.Run.ID, Name: request.JobName,
		Repo: request.Repository.Name, UpstreamName: request.UpstreamName, UpstreamOrg: request.UpstreamOrg,
		Ref: request.Run.Ref, SHA: testedSHA, Trigger: trigger, Source: source,
		SourceDir: request.SourceDir, ApprovedSecrets: approvedSecrets,
		Fragments: fragments,
	}
	if preparer, ok := jobs.controller.(interface {
		PrepareNonroot(context.Context, argojob.Request) (argojob.Request, error)
	}); ok {
		submission, err = preparer.PrepareNonroot(ctx, submission)
		if err != nil {
			return err
		}
	}
	if err := jobs.auditSubmission(ctx, request, submission); err != nil {
		return err
	}
	createdName, err := jobs.controller.Create(ctx, submission)
	if err != nil {
		return err
	}
	if createdName != request.JobName {
		_ = jobs.controller.Cancel(ctx, createdName, request.Run.ID)
		return fmt.Errorf("app: Argo created Workflow %q, expected %q", createdName, request.JobName)
	}

	// Post-create check: if Delete arrived while controller.Create was
	// running, the live Workflow must be cancelled. The intent is removed
	// here because Delete left the tombstone for create() to clean up.
	jobs.mu.Lock()
	if intent, ok := jobs.intents[request.JobName]; ok && intent.cancelled {
		delete(jobs.intents, request.JobName)
		jobs.mu.Unlock()
		_ = jobs.controller.Cancel(ctx, createdName, request.Run.ID)
		return ErrSuperseded
	}
	// Mark the intent as created so Delete (and Wait) know the Workflow
	// exists and can be cancelled via the controller.
	if intent, ok := jobs.intents[request.JobName]; ok && intent.runID == request.Run.ID {
		intent.created = true
		jobs.intents[request.JobName] = intent
	}
	jobs.mu.Unlock()

	success = true
	return nil
}

// auditSubmission records the exact identity binding this run will execute
// under, before the Workflow object exists.
//
// On this tier Oberth never holds the secret values: envconsul fetches them
// in-Pod, so there is nothing for the server to mask or log. The audit chain's
// contribution is therefore the binding itself -- which run, which trigger,
// which namespace, which ServiceAccount, and which paths the document declared
// it would read. Joined with OpenBao's own audit device, that is what makes an
// access attributable to a push.
func (jobs *ArgoJobs) auditSubmission(ctx context.Context, request service.JobRequest, submission argojob.Request) error {
	actor := strings.TrimSpace(request.Run.Actor)
	if actor == "" {
		return errors.New("app: Workflow submission requires an attributable uplink identity")
	}
	workflow, err := argojob.Build(jobs.config, submission)
	if err != nil {
		return err
	}
	declared := ""
	if workflow.Annotations != nil {
		declared = workflow.Annotations["oberth.ci/declared-secret-paths"]
	}
	fragments := ""
	if workflow.Annotations != nil {
		fragments = workflow.Annotations[argojob.FragmentsAnnotation]
	}
	automount := workflow.Spec.AutomountServiceAccountToken != nil && *workflow.Spec.AutomountServiceAccountToken
	details := map[string]any{
		"repo": request.Repository.Name, "upstream_org": request.UpstreamOrg,
		"ref": request.Run.Ref,
		// "sha" is the commit the Workflow actually executes (submission.SHA,
		// derived from TestedSHA). "object_sha" preserves the original pushed
		// object for provenance: for annotated tags these differ because the
		// pushed OID is the tag object, not the commit.
		"sha": submission.SHA, "object_sha": request.Run.SHA,
		"trigger": string(submission.Trigger),
		"engine":  "argo", "workflow": submission.Name,
		"namespace":       workflow.Namespace,
		"service_account": workflow.Spec.ServiceAccountName,
		"executor_service_account": func() string {
			if workflow.Spec.Executor == nil {
				return ""
			}
			return workflow.Spec.Executor.ServiceAccountName
		}(),
		"automount_service_account_token": automount,
		"declared_secret_paths":           declared,
		"fragments":                       fragments,
	}
	if binding := workflow.Annotations["oberth.ci/verified-controller-profile"]; binding != "" {
		details["controller_profile"] = binding
		details["nonroot_templates"] = workflow.Annotations[argoworkflow.NonrootTemplatesAnnotation]
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("app: encode Workflow submission audit details: %w", err)
	}
	if _, err := jobs.auditor.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor: actor, Action: string(submission.Trigger) + ".argo.submit.binding",
		ResourceType: "run", ResourceID: request.Run.ID, Details: string(encoded),
	}); err != nil {
		return fmt.Errorf("app: persist Workflow submission binding: %w", err)
	}
	return nil
}

func (jobs *ArgoJobs) Wait(ctx context.Context, name string, destination io.Writer) (service.JobResult, error) {
	jobs.mu.Lock()
	intent, ok := jobs.intents[name]
	jobs.mu.Unlock()
	if !ok {
		return service.JobResult{}, fmt.Errorf("app: no in-process intent for Workflow %s", name)
	}
	defer jobs.forget(name, intent.runID)
	completion, err := jobs.controller.Wait(ctx, name, intent.runID, destination)
	if err != nil {
		return service.JobResult{}, err
	}
	if reason := jobs.collectArtifacts(context.WithoutCancel(ctx), name, intent.runID, intent.trigger); reason != "" {
		jobs.recordArtifactFailure(context.WithoutCancel(ctx), intent.runID, reason)
	}
	return jobs.result(completion), nil
}

func (jobs *ArgoJobs) Delete(ctx context.Context, name, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return errors.New("app: durable run ID is required to delete a Workflow")
	}

	jobs.mu.Lock()
	if current, ok := jobs.intents[name]; ok && current.runID != runID {
		jobs.mu.Unlock()
		return fmt.Errorf("app: Workflow %s belongs to a different in-process run", name)
	}
	if current, ok := jobs.intents[name]; ok && !current.created {
		// The Workflow does not exist yet — create() is still running
		// (seeding, auditing, or waiting on controller.Create). Mark the
		// intent cancelled so create() will either abort early or cancel
		// the Workflow after controller.Create returns. Leave the intent
		// in the map: create() owns the final removal.
		current.cancelled = true
		jobs.intents[name] = current
		jobs.mu.Unlock()
		// Attempt Cancel anyway — harmless NotFound if the Workflow
		// genuinely does not exist yet, and a safety net if the Workflow
		// was created between our read of created=false and this call.
		_ = jobs.controller.Cancel(ctx, name, runID)
		return nil
	}
	jobs.mu.Unlock()

	// Intent is either created=true (workflow exists) or absent (no
	// in-flight create). Cancel the controller object, then remove the
	// intent.
	if err := jobs.controller.Cancel(ctx, name, runID); err != nil {
		return err
	}
	jobs.mu.Lock()
	if current, ok := jobs.intents[name]; ok && current.runID == runID {
		delete(jobs.intents, name)
	}
	jobs.mu.Unlock()
	return nil
}

// TerminalResult reconstructs a run's outcome from the Workflow object alone,
// so a server that restarted mid-run records what really happened.
func (jobs *ArgoJobs) TerminalResult(ctx context.Context, name string) (service.JobResult, error) {
	completion, err := jobs.controller.TerminalState(ctx, name)
	if errors.Is(err, argojob.ErrNotTerminal) {
		return service.JobResult{}, service.ErrJobNotTerminal
	}
	if err != nil {
		return service.JobResult{}, err
	}
	if completion == nil {
		return service.JobResult{}, service.ErrJobNotTerminal
	}
	return jobs.result(*completion), nil
}

// Owns reports whether this engine created the named object. The dispatcher
// uses it to route a name back to its engine when no in-process state survives.
func (jobs *ArgoJobs) Owns(ctx context.Context, name string) (bool, error) {
	return jobs.controller.Owns(ctx, name)
}

func (jobs *ArgoJobs) forget(name, runID string) {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	if current, ok := jobs.intents[name]; ok && current.runID == runID {
		delete(jobs.intents, name)
	}
}

// result maps the engine's completion onto the same durable JobResult the Job
// engine produces, so nothing downstream can tell which engine ran.
func (jobs *ArgoJobs) result(completion argojob.Completion) service.JobResult {
	result := service.JobResult{Status: model.RunFailed, Phase: "job"}
	if completion.Succeeded {
		result.Status = model.RunPassed
		result.Phase = "passed"
	} else {
		result.Error = completion.Reason
		if result.Error == "" {
			result.Error = "Workflow " + completion.Phase
		}
	}
	result.Steps = make([]model.StepResult, 0, len(completion.Steps))
	for _, step := range completion.Steps {
		result.Steps = append(result.Steps, model.StepResult{
			Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal,
			Status: model.StepStatus(step.Status), ExitCode: step.ExitCode,
			StartedAt: step.StartedAt, FinishedAt: step.FinishedAt,
		})
	}
	if completion.FailedStep != "" {
		result.FailedBurn, result.FailedStep = completion.FailedBurn, completion.FailedStep
		result.Phase = completion.FailedBurn
	}
	if result.Status == model.RunPassed && result.FailedStep != "" {
		result.Status = model.RunFailed
		result.Phase = result.FailedBurn
		result.Error = "successful Workflow reported a failed step"
	}
	return result
}

// readArgoSource reads the pipeline document for one trigger out of the
// immutable run workspace, through the same bounded, root-confined reader the
// Go path uses for periapsis.go.
func readArgoSource(sourceDir string, trigger periapsis.Trigger) ([]byte, error) {
	file, err := argoworkflow.TriggerFile(trigger)
	if err != nil {
		return nil, err
	}
	return readBoundedSourceFile(sourceDir, file)
}

// noPipelineError returns a user-facing error when the repository has no
// pipeline configuration file for the given trigger. If the underlying error
// is not os.ErrNotExist it is returned unchanged so callers can use this as a
// pass-through wrapper.
func noPipelineError(err error, trigger periapsis.Trigger, repoName string) error {
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, _ := argoworkflow.TriggerFile(trigger)
	if file == "" {
		file = argoworkflow.BuildFile
	}
	return fmt.Errorf("repository %q has no pipeline configuration (%s); run \"oberth init\" in the repository to generate one", repoName, file)
}

// PipelineCredentialed reports whether the checked-out pipeline declares
// secret-store paths by decoding the same document PipelineSize reads.
func (jobs *ArgoJobs) PipelineCredentialed(_ context.Context, request service.JobRequest) (bool, error) {
	trigger, err := runTrigger(request.Run)
	if err != nil {
		return false, err
	}
	source, err := readArgoSource(request.SourceDir, trigger)
	if err != nil {
		return false, noPipelineError(err, trigger, request.Repository.Name)
	}
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return false, nil
	}
	paths, err := argoworkflow.DeclaredSecretPaths(workflow)
	return err == nil && len(paths) > 0, nil
}

var (
	_ service.JobController              = (*ArgoJobs)(nil)
	_ service.ReleaseJobController       = (*ArgoJobs)(nil)
	_ service.PipelineSizer              = (*ArgoJobs)(nil)
	_ service.PipelinePlanner            = (*ArgoJobs)(nil)
	_ service.PipelineCredentialDetector = (*ArgoJobs)(nil)
	_                                    = time.Second
)
