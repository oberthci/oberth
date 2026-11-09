package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// argoControl is the engine seam, narrowed the same way jobControl narrows the
// Argo Workflow engine.
type argoControl interface {
	Create(context.Context, argojob.Request) (string, error)
	Wait(context.Context, string, string, io.Writer) (argojob.Completion, error)
	Cancel(context.Context, string, string) error
	TerminalState(context.Context, string) (*argojob.Completion, error)
	Owns(context.Context, string) (bool, error)
	Namespace() string
}

func (jobs *ArgoJobs) WriteResourceMetrics(w io.Writer) {
	if controller, ok := jobs.controller.(interface{ WriteResourceMetrics(io.Writer) }); ok {
		controller.WriteResourceMetrics(w)
	}
	if jobs.routes != nil {
		for _, definition := range jobs.routes.definitions {
			if source, ok := definition.ResourceSource.(interface{ WriteMetrics(io.Writer) }); ok {
				source.WriteMetrics(w)
			}
		}
	}
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

// DeclarationRecorder records (repo, step, path) declarations observed at
// Workflow admission so `access_list --pending` can surface declared-but-not-
// granted paths without reading the git cache at query time.
// Issue #623, finding 4.
type DeclarationRecorder interface {
	RecordGrantDeclarations(ctx context.Context, repo, sha string, declarations []store.GrantDeclaration) error
}

// ArgoJobs adapts the Argo execution engine to the same control-plane
// contracts the Argo Workflow engine satisfies, so the scheduler, admission
// gate, store, and audit chain need no knowledge of which engine ran a run.
type ArgoJobs struct {
	pinnedSources  *PinnedSourceLoader
	sourceRecorder PinnedSourceRecorder

	routes        *executionTargets
	targetBinding *model.ExecutionTargetBinding
	targetSource  []byte
	targetClients *argojob.TargetClients
	beforeCreate  func(context.Context, service.WorkflowRequest, argojob.Request) error

	capabilityGrants  func(context.Context, int64) (*argoworkflow.Capability, error)
	resourceBlobs     workflowpolicy.PolicyGit
	controller        argoControl
	auditor           service.Auditor
	config            argojob.Config
	secretAccess      SecretAccessLoader
	declarations      DeclarationRecorder
	fragments         FragmentLoader
	collector         ArtifactCollector
	artifacts         ArtifactStore
	artifactLimit     int64
	reconcilerHealthy ReconcilerHealthChecker

	// identities holds the live per-repo identity maps. When non-nil,
	// create() snapshots it once per admission so both Builds (audit and
	// controller) bind the same identity version. Updated by the
	// reconciler callback in serve.go. (Issue #465)
	identities *argojob.IdentityStore
	// releaseWIF layers audited runtime overrides on the boot release WIF
	// map (#1497). Each admission pins one snapshot for both Builds.
	releaseWIF *argojob.ReleaseWIFRuntime

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
	sources   []pipeline.PinnedSource
	adoption  *argojob.Adoption
	runID     string
	trigger   pipeline.Trigger
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
	if jobs.routes != nil {
		jobs.routes.local.SetReconcilerHealth(checker)
	}
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.reconcilerHealthy = checker
}

func NewArgoJobs(controller argoControl, config argojob.Config, auditor service.Auditor, secretAccess SecretAccessLoader, fragments FragmentLoader) (*ArgoJobs, error) {
	config.ReleaseWIF = config.ReleaseWIF.Clone()
	config.ReleaseTagRoutes = config.ReleaseTagRoutes.Clone()
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
	jobs := &ArgoJobs{controller: controller, auditor: auditor, config: config, secretAccess: secretAccess, fragments: fragments, intents: map[string]argoIntent{}}
	return jobs, nil
}

// SetDeclarationRecorder wires the grant declaration recorder after
// construction. When set, create() records (repo, step, path) declarations
// at Workflow admission for `access_list --pending`. Issue #623, finding 4.
func (jobs *ArgoJobs) SetDeclarationRecorder(recorder DeclarationRecorder) {
	if jobs.routes != nil {
		jobs.routes.local.SetDeclarationRecorder(recorder)
	}
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.declarations = recorder
}

// SetWorkflowCapabilityLoader wires durable operator authority before serving
// requests. The controller independently rechecks it at Kubernetes effects.
func (jobs *ArgoJobs) SetWorkflowCapabilityLoader(load func(context.Context, int64) (*argoworkflow.Capability, error)) {
	if jobs.routes != nil {
		jobs.routes.local.SetWorkflowCapabilityLoader(load)
	}
	jobs.capabilityGrants = load
	if controller, ok := jobs.controller.(interface {
		WithWorkflowCapabilityLoader(func(context.Context, int64) (*argoworkflow.Capability, error)) *argojob.Controller
	}); ok {
		controller.WithWorkflowCapabilityLoader(load)
	}
}

// SetIdentityStore wires the live identity store after construction.
// When set, create() snapshots it once per admission so both the audit Build
// and the controller Build bind the same identity version. (Issue #465)
func (jobs *ArgoJobs) SetIdentityStore(store *argojob.IdentityStore) {
	if jobs.routes != nil {
		jobs.routes.local.SetIdentityStore(store)
	}
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.identities = store
}

// SetReleaseWIFRuntime wires the runtime release WIF layer (#1497) before
// serving requests. Without it admissions use the boot map unchanged.
func (jobs *ArgoJobs) SetReleaseWIFRuntime(runtime *argojob.ReleaseWIFRuntime) {
	if jobs.routes != nil {
		jobs.routes.local.SetReleaseWIFRuntime(runtime)
	}
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.releaseWIF = runtime
}

// ReleaseWIFRuntime returns the wired runtime layer, or nil.
func (jobs *ArgoJobs) ReleaseWIFRuntime() *argojob.ReleaseWIFRuntime {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	return jobs.releaseWIF
}

func (jobs *ArgoJobs) releaseWIFSnapshot() *argojob.ReleaseWIFConfig {
	jobs.mu.Lock()
	runtime := jobs.releaseWIF
	jobs.mu.Unlock()
	if runtime == nil {
		return nil
	}
	return runtime.Snapshot()
}

func (jobs *ArgoJobs) CreateWorkflow(ctx context.Context, request service.WorkflowRequest) error {
	return jobs.create(ctx, request, pipeline.TriggerCI)
}

func (jobs *ArgoJobs) CreateRelease(ctx context.Context, request service.WorkflowRequest) error {
	return jobs.create(ctx, request, pipeline.TriggerRelease)
}

// PipelineSize reads the run's declared scheduler weight from the checked-out
// document without evaluating anything: the size-by-trigger entry for this
// run's own trigger when the document names one, oberth.ci/size otherwise.
func (jobs *ArgoJobs) PipelineSize(ctx context.Context, request service.WorkflowRequest) (pipeline.Size, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forRequest(ctx, request)
		if err != nil {
			return "", err
		}
		return child.PipelineSize(ctx, request)
	}
	source, err := jobs.readRunSource(request)
	if err != nil {
		return "", jobs.noRunPipelineError(err, request)
	}
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return "", fmt.Errorf("app: read repository resource contract: %w", err)
	}
	kind, err := runKind(request.Run)
	if err != nil {
		return "", err
	}
	size, err := argoworkflow.DeclaredSizeFor(workflow, kind)
	if err != nil {
		return "", fmt.Errorf("app: read repository resource contract: %w", err)
	}
	return size, nil
}

// PipelineConcurrencyGroup reads the run's declared concurrency group (#658)
// from the same checked-out document PipelineSize reads, without evaluating
// anything. Equal group names share admission across repositories.
func (jobs *ArgoJobs) PipelineConcurrencyGroup(ctx context.Context, request service.WorkflowRequest) (string, error) {
	group, _, err := jobs.PipelineConcurrencyContract(ctx, request)
	return group, err
}

// PipelineConcurrencyContract reads both declarations from the same immutable document.
func (jobs *ArgoJobs) PipelineConcurrencyContract(ctx context.Context, request service.WorkflowRequest) (string, int, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forRequest(ctx, request)
		if err != nil {
			return "", 0, err
		}
		return child.PipelineConcurrencyContract(ctx, request)
	}
	source, err := jobs.readRunSource(request)
	if err != nil {
		return "", 0, jobs.noRunPipelineError(err, request)
	}
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return "", 0, fmt.Errorf("app: read repository concurrency contract: %w", err)
	}
	group, err := argoworkflow.DeclaredConcurrencyGroup(workflow)
	if err != nil {
		return "", 0, err
	}
	limit, err := argoworkflow.DeclaredConcurrencyLimit(workflow)
	return group, limit, err
}

// PipelineExclusiveClasses reads the exclusive classes the run's steps
// declare (#1499) from the same immutable document, without evaluating it.
func (jobs *ArgoJobs) PipelineExclusiveClasses(ctx context.Context, request service.WorkflowRequest) ([]string, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forRequest(ctx, request)
		if err != nil {
			return nil, err
		}
		return child.PipelineExclusiveClasses(ctx, request)
	}
	source, err := jobs.readRunSource(request)
	if err != nil {
		return nil, jobs.noRunPipelineError(err, request)
	}
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return nil, fmt.Errorf("app: read repository exclusive classes: %w", err)
	}
	return argoworkflow.DeclaredExclusiveClasses(workflow)
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
func (jobs *ArgoJobs) PlannedSteps(ctx context.Context, request service.WorkflowRequest) ([]runprogress.PlannedStep, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forRequest(ctx, request)
		if err != nil {
			return nil, err
		}
		return child.PlannedSteps(ctx, request)
	}
	source, err := jobs.readRunSource(request)
	if err != nil {
		return nil, jobs.noRunPipelineError(err, request)
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
		steps = append(steps, runprogress.PlannedStep{Stage: step.Stage, Step: step.Step, Ordinal: step.Ordinal, Secrets: step.Secrets, Credentialed: step.Credentialed})
	}
	return steps, nil
}

func (jobs *ArgoJobs) create(ctx context.Context, request service.WorkflowRequest, trigger pipeline.Trigger) error {
	if jobs.routes != nil {
		child, err := jobs.routes.forRequest(ctx, request)
		if err != nil {
			return err
		}
		err = child.create(ctx, request, trigger)
		if err != nil && child.targetBinding != nil {
			return fmt.Errorf("%w: %w", argojob.ErrTargetUncertain, err)
		}
		return err
	}
	var branchAuthority *workflowpolicy.BranchAuthority
	if jobs.targetBinding != nil {
		if err := jobs.config.ExecutionPlacement.ValidateAdmission(); err != nil {
			return err
		}
		b := jobs.targetBinding
		if err := matchTargetRun(*b, request.Run, request.WorkflowName); err != nil {
			return err
		}
		var err error
		branchAuthority, err = workflowpolicy.ResolveBranchAuthority(ctx, jobs.resourceBlobs, b.Repository, b.TestedSHA)
		if err != nil {
			return err
		}
		if !branchAuthority.Matches(b.Repository, b.TestedSHA, b.PolicyBaseline, b.PolicyDigest) {
			return errors.New("app: branch policy changed before target admission")
		}
	}

	if strings.TrimSpace(request.WorkflowName) == "" || strings.TrimSpace(request.Run.ID) == "" || request.Repository.Name == "" {
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
	// held only for the in-memory map check, not for source reads or
	// audit/seeding/controller calls.
	jobs.mu.Lock()
	if existing, occupied := jobs.intents[request.WorkflowName]; occupied {
		jobs.mu.Unlock()
		if existing.runID == request.Run.ID {
			return nil // same submission already in flight
		}
		return fmt.Errorf("app: Workflow %s is in flight for a different run %s", request.WorkflowName, existing.runID)
	}
	jobs.intents[request.WorkflowName] = argoIntent{runID: request.Run.ID, trigger: trigger}
	jobs.mu.Unlock()

	// Everything below runs outside the lock. On failure, remove the
	// reservation so a retry or a different run can claim the name —
	// UNLESS Delete has already marked the intent cancelled, in which case
	// Delete owns the intent lifecycle and this defer must not remove it.
	success := false
	defer func() {
		if !success {
			jobs.mu.Lock()
			if current, ok := jobs.intents[request.WorkflowName]; ok && current.runID == request.Run.ID && !current.cancelled {
				delete(jobs.intents, request.WorkflowName)
			}
			jobs.mu.Unlock()
		}
	}()

	// Early abort: Delete arrived between reservation and seeding.
	jobs.mu.Lock()
	if jobs.intents[request.WorkflowName].cancelled {
		// Delete already marked the intent cancelled and owns cleanup.
		// Remove the tombstone now — no workflow exists to cancel.
		delete(jobs.intents, request.WorkflowName)
		jobs.mu.Unlock()
		return ErrSuperseded
	}
	jobs.mu.Unlock()

	source, err := jobs.readRunSource(request)
	if err != nil {
		return jobs.noRunPipelineError(err, request)
	}
	testedSHA := request.Run.TestedSHA
	if testedSHA == "" {
		testedSHA = request.Run.SHA
	}
	var fragments map[argoworkflow.FragmentKey]argoworkflow.Fragment
	if request.DemandReceipt != nil {
		fragments = request.DemandReceipt.Fragments()
	} else {
		fragments, err = loadFragments(ctx, jobs.fragments, source, trigger, request.Run.RefKind, request.Run.Trigger)
		if err != nil {
			return fmt.Errorf("app: resolve fragments for %s: %w", request.Repository.Name, err)
		}
	}
	kind, err := runKind(request.Run)
	if err != nil {
		return err
	}
	pipelineFile, err := jobs.runSourceFile(request)
	if err != nil {
		return err
	}
	// Classify the same resolved graph Build will admit, including inline
	// templates and pinned fragments. Durable flags remain conservative:
	// a release or a run already marked credentialed cannot downgrade itself.
	resolved, err := argoworkflow.ResolveRunSource(source, fragments, argoworkflow.RunSourceBinding{
		RunID: request.Run.ID, RepositoryID: request.Repository.ID, SHA: testedSHA,
		File: pipelineFile, Trigger: kind, ScheduleEntry: request.Run.ScheduleEntry,
	})
	if err != nil {
		return err
	}
	declaresCredentials, err := workflowCredentialed(resolved.Workflow())
	if err != nil {
		return err
	}
	credentialed := trigger == pipeline.TriggerRelease || request.Run.Credentialed || request.Run.Release || declaresCredentials
	jobs.mu.Lock()
	blocked := credentialed && jobs.secretAccess != nil && jobs.reconcilerHealthy != nil && !jobs.reconcilerHealthy.ReconcileHealthy()
	jobs.mu.Unlock()
	if blocked {
		return fmt.Errorf("app: access reconciler has not completed initial reconciliation; credentialed admission is blocked for %s", request.Repository.Name)
	}

	// Uncredentialed runs carry an empty approval set and never read grants.
	// Credentialed runs still require a loaded approval table, even when the
	// reconciler is healthy; nil makes Build refuse a missing loader.
	var approvedSecrets map[string]bool
	if !credentialed {
		approvedSecrets = make(map[string]bool)
	}
	var stepGrants map[string]map[string]bool
	if credentialed && jobs.secretAccess != nil {
		grants, loadErr := jobs.secretAccess.ActiveSecretGrants(ctx, request.Repository.ID)
		if loadErr != nil {
			return fmt.Errorf("app: load approved secrets for %s: %w", request.Repository.Name, loadErr)
		}
		approvedSecrets = make(map[string]bool)
		stepGrants = make(map[string]map[string]bool)
		for step, secrets := range grants {
			for secret := range secrets {
				approvedSecrets[secret] = true
				if stepGrants[secret] == nil {
					stepGrants[secret] = make(map[string]bool)
				}
				stepGrants[secret][step] = true
			}
		}
	}
	submission := argojob.Request{
		BranchAuthority:  branchAuthority,
		PodDemand:        request.PodDemand,
		Exclusive:        request.Exclusive,
		ResourceFallback: request.ResourceFallback,
		DemandReceipt:    request.DemandReceipt,
		PipelineFile:     pipelineFile,
		RunID:            request.Run.ID, RepositoryID: request.Repository.ID, Name: request.WorkflowName,
		Repo: request.Repository.Name, UpstreamName: request.UpstreamName, UpstreamOrg: request.UpstreamOrg,
		Ref: request.Run.Ref, SHA: testedSHA, Trigger: trigger, RunTrigger: kind, ScheduleEntry: request.Run.ScheduleEntry, Source: source,
		Assist:    request.Run.AssistRequested,
		SourceDir: request.SourceDir, ApprovedSecrets: approvedSecrets,
		StepGrants: stepGrants,
		Fragments:  fragments,
	}

	sources, err := argoworkflow.PinnedSources(resolved.Workflow())
	if err != nil {
		return err
	}
	trees, cleanup, err := jobs.pinnedSources.prepare(ctx, model.CanonicalRepoKey(request.UpstreamName, request.UpstreamOrg, request.Repository.Name), filepath.Dir(request.SourceDir), sources)
	if err != nil {
		return err
	}
	defer cleanup()
	submission.PinnedSources = trees
	jobs.mu.Lock()
	pinnedIntent := jobs.intents[request.WorkflowName]
	pinnedIntent.sources = sources
	jobs.intents[request.WorkflowName] = pinnedIntent
	jobs.mu.Unlock()
	if len(sources) > 0 && jobs.sourceRecorder == nil {
		return errors.New("app: pinned sources require durable run recording")
	}
	if jobs.capabilityGrants != nil {
		submission.Capability, err = jobs.capabilityGrants(ctx, request.Repository.ID)
		if err != nil {
			return err
		}
	}
	if err := jobs.bindResourceMaximum(ctx, &submission); err != nil {
		return err
	}
	// Snapshot the identity store once for both Builds of this admission
	// (audit and controller), so the audit record and the pod spec agree
	// on which ServiceAccount the run executes under. (Issue #465)
	if jobs.identities != nil {
		submission.Identities = jobs.identities.Snapshot()
	}
	submission.ReleaseWIF = jobs.releaseWIFSnapshot()
	if preparer, ok := jobs.controller.(interface {
		PrepareNonroot(context.Context, argojob.Request) (argojob.Request, error)
	}); ok {
		submission, err = preparer.PrepareNonroot(ctx, submission)
		if err != nil {
			return err
		}
	}
	if len(sources) > 0 {
		if err := jobs.sourceRecorder.RecordRunSources(ctx, request.Run.ID, testedSHA, sources); err != nil {
			return err
		}
	}
	if err := jobs.auditSubmission(ctx, request, submission); err != nil {
		return err
	}
	// Record (repo, step, path) declarations for `access_list --pending`.
	// Best-effort: failures are logged and never block admission.
	// Issue #623, finding 4.
	jobs.recordDeclarations(ctx, request, source, trigger)
	jobs.recordCurrentDeclarations(ctx, request)

	if jobs.beforeCreate != nil {
		if err := jobs.beforeCreate(ctx, request, submission); err != nil {
			return err
		}
	}
	createdName, err := jobs.controller.Create(ctx, submission)
	if err != nil {
		return err
	}
	if createdName != request.WorkflowName {
		_ = jobs.controller.Cancel(ctx, createdName, request.Run.ID)
		return fmt.Errorf("app: Argo created Workflow %q, expected %q", createdName, request.WorkflowName)
	}

	// Post-create check: if Delete arrived while controller.Create was
	// running, the live Workflow must be cancelled. The intent is removed
	// here because Delete left the tombstone for create() to clean up.
	jobs.mu.Lock()
	if intent, ok := jobs.intents[request.WorkflowName]; ok && intent.cancelled {
		delete(jobs.intents, request.WorkflowName)
		jobs.mu.Unlock()
		if jobs.targetBinding == nil {
			_ = jobs.controller.Cancel(ctx, createdName, request.Run.ID)
		}
		return ErrSuperseded
	}
	// Mark the intent as created so Delete (and Wait) know the Workflow
	// exists and can be cancelled via the controller.
	if intent, ok := jobs.intents[request.WorkflowName]; ok && intent.runID == request.Run.ID {
		intent.created = true
		jobs.intents[request.WorkflowName] = intent
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
func (jobs *ArgoJobs) auditSubmission(ctx context.Context, request service.WorkflowRequest, submission argojob.Request) error {
	actor := strings.TrimSpace(request.Run.Actor)
	if actor == "" {
		return errors.New("app: Workflow submission requires an attributable uplink identity")
	}
	workflow, err := argojob.Build(jobs.config, submission)
	if err != nil {
		return err
	}
	file, err := jobs.runSourceFile(request)
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
		"resource_maximum": submission.ResourceMaximum,
		"resource_ceiling": submission.ResourceCeiling,
		"repo":             request.Repository.Name, "upstream_org": request.UpstreamOrg,
		"ref": request.Run.Ref,
		// "sha" is the commit the Workflow actually executes (submission.SHA,
		// derived from TestedSHA). "object_sha" preserves the original pushed
		// object for provenance: for annotated tags these differ because the
		// pushed OID is the tag object, not the commit.
		"sha": submission.SHA, "object_sha": request.Run.SHA,
		"trigger": string(submission.Trigger),
		"engine":  "argo", "workflow": submission.Name, "pipeline_file": file,
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
		"sources":                         sourcesForAudit(submission.PinnedSources),
	}
	if submission.DemandReceipt != nil {
		details["source_receipt"] = submission.DemandReceipt.Summary()
	}
	if binding := workflow.Annotations["oberth.ci/verified-controller-profile"]; binding != "" {
		details["controller_profile"] = binding
		details["nonroot_templates"] = workflow.Annotations[argoworkflow.NonrootTemplatesAnnotation]
	}
	if policy := workflow.Annotations["oberth.ci/release-wif-policy-sha256"]; policy != "" {
		details["release_wif_policy_sha256"] = policy
		details["workflow_spec_sha256"] = workflow.Annotations["oberth.ci/spec-identity"]
		leaves := map[string]string{}
		for _, leaf := range workflow.Spec.Templates {
			if role := leaf.Metadata.Annotations[argoworkflow.ReleaseWIFRoleAnnotation]; role != "" {
				leaves[leaf.Name] = role
			}
		}
		details["release_wif_leaves"] = leaves
	}
	if request.ConcurrencyGroup != "" {
		// The group the scheduler actually admitted this run under (#658),
		// not a re-read of the document: the audit records the decision.
		details["concurrency_group"] = request.ConcurrencyGroup
		details["concurrency_limit"] = max(1, request.Run.ConcurrencyLimit)
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

// recordDeclarations extracts (step, path) declarations from the workflow
// source and records them for `access_list --pending`. Best-effort: failures
// are logged to the run context and never block admission. Issue #623, finding 4.
func (jobs *ArgoJobs) recordDeclarations(ctx context.Context, request service.WorkflowRequest, source []byte, trigger pipeline.Trigger) {
	if jobs.declarations == nil {
		return
	}
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return // Decode failure: Build will surface the real error.
	}
	paths, err := argoworkflow.DeclaredSecretPaths(workflow)
	if err != nil || len(paths) == 0 {
		return
	}
	// Walk templates to extract (template_name, path) pairs from
	// `oberth secretstore exec --path` invocations.
	var declarations []store.GrantDeclaration
	for i := range workflow.Spec.Templates {
		tmpl := &workflow.Spec.Templates[i]
		if tmpl.Name == "" {
			continue
		}
		execPaths := argojob.ExtractExecPaths(tmpl)
		for _, p := range execPaths {
			declarations = append(declarations, store.GrantDeclaration{
				Step: tmpl.Name,
				Path: p,
			})
		}
	}
	// Also record paths from the annotation with wildcard step for templates
	// that use envconsul (legacy chain) or other patterns not captured above.
	declared := make(map[string]bool, len(declarations))
	for _, d := range declarations {
		declared[d.Step+"\x00"+d.Path] = true
	}
	for _, p := range paths {
		key := "*\x00" + p
		if !declared[key] {
			// Check if any template already declares this path.
			found := false
			for _, d := range declarations {
				if d.Path == p {
					found = true
					break
				}
			}
			if !found {
				declarations = append(declarations, store.GrantDeclaration{Step: "*", Path: p})
			}
		}
	}
	if len(declarations) == 0 {
		return
	}
	qualifiedRepo := declarationRepo(request)
	sha := request.Run.TestedSHA
	if sha == "" {
		sha = request.Run.SHA
	}
	// Best-effort: log and continue on error.
	if err := jobs.declarations.RecordGrantDeclarations(ctx, qualifiedRepo, sha, declarations); err != nil {
		_ = err // Logged and never blocks admission.
	}
}

// Adopt rebuilds the intent from the durable run and the existing Workflow.
// It never seeds sources, submits work, or re-evaluates repository commands.
func (jobs *ArgoJobs) Adopt(ctx context.Context, run model.Run) (pipeline.Size, bool, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forName(ctx, run.WorkflowName, run.ID)
		if err != nil {
			return "", false, err
		}
		size, found, err := child.Adopt(ctx, run)
		if err == nil && !found && child.targetBinding != nil {
			return "", false, argojob.ErrTargetUncertain
		}
		return size, found, err
	}
	controller, ok := jobs.controller.(interface {
		Adopt(context.Context, argojob.AdoptionRequest) (argojob.Adoption, bool, error)
	})
	if !ok {
		return "", false, errors.New("app: controller cannot adopt Workflows")
	}
	trigger, err := runTrigger(run)
	if err != nil {
		return "", false, err
	}
	kind, err := runKind(run)
	if err != nil {
		return "", false, err
	}
	sha := run.TestedSHA
	if sha == "" {
		sha = run.SHA
	}
	binding, found, err := controller.Adopt(ctx, argojob.AdoptionRequest{
		PinnedSources: run.SourceBindings, RepositoryID: run.RepoID, Name: run.WorkflowName, RunID: run.ID, Ref: run.Ref, SHA: sha,
		Group: run.ConcurrencyGroup, Trigger: trigger, RunTrigger: kind, ScheduleEntry: run.ScheduleEntry,
	})
	if errors.Is(err, argojob.ErrAdoptionRefused) {
		return "", false, fmt.Errorf("%w: %w", service.ErrAdoptionRefused, err)
	}
	if err != nil || !found {
		return "", found, err
	}
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	if current, exists := jobs.intents[run.WorkflowName]; exists {
		if current.runID != run.ID || current.cancelled || current.adoption == nil || !reflect.DeepEqual(*current.adoption, binding) {
			return "", false, fmt.Errorf("%w: app: conflicting Workflow adoption intent", service.ErrAdoptionRefused)
		}
		return binding.Size, true, nil
	}
	jobs.intents[run.WorkflowName] = argoIntent{runID: run.ID, trigger: trigger, created: true, adoption: &binding, sources: run.SourceBindings}
	return binding.Size, true, nil
}

// WorkflowNamespace names the namespace the engine's Workflows live in.
func (jobs *ArgoJobs) WorkflowNamespace() string {
	return jobs.controller.Namespace()
}

func (jobs *ArgoJobs) WaitResumed(ctx context.Context, name string, destination io.Writer, events []runprogress.Event, retained int64) (service.JobResult, error) {
	return jobs.wait(ctx, name, destination, events, retained)
}

func (jobs *ArgoJobs) Wait(ctx context.Context, name string, destination io.Writer) (service.JobResult, error) {
	return jobs.wait(ctx, name, destination, nil, 0)
}

func (jobs *ArgoJobs) wait(ctx context.Context, name string, destination io.Writer, events []runprogress.Event, retained int64) (service.JobResult, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forName(ctx, name, "")
		if err != nil {
			return service.JobResult{}, err
		}
		result, err := child.wait(ctx, name, destination, events, retained)
		if child.targetClients != nil && err == nil {
			if custodyErr := child.targetClients.VerifyResultCustody(ctx); custodyErr != nil {
				return service.JobResult{}, custodyErr
			}
		}
		if result.Reobserve != nil && child.targetClients != nil {
			original := result.Reobserve
			result.Reobserve = func(ctx context.Context) (service.JobResult, error) {
				if err := child.targetClients.VerifyResultCustody(ctx); err != nil {
					return service.JobResult{}, err
				}
				return original(ctx)
			}
		}
		return result, err
	}

	jobs.mu.Lock()
	intent, ok := jobs.intents[name]
	jobs.mu.Unlock()
	if !ok {
		return service.JobResult{}, fmt.Errorf("app: no in-process intent for Workflow %s", name)
	}
	defer jobs.forget(name, intent.runID)
	if retained == 0 {
		for _, source := range intent.sources {
			if _, err := fmt.Fprintf(destination, "source %s@%s mounted read-only at %s\n", source.Repository, source.Commit, source.Path); err != nil {
				return service.JobResult{}, err
			}
		}
	}

	var completion argojob.Completion
	var err error

	if intent.adoption != nil {
		controller, ok := jobs.controller.(interface {
			WaitResumed(context.Context, string, string, io.Writer, []runprogress.Event, int64, argojob.Adoption) (argojob.Completion, error)
		})
		if !ok {
			return service.JobResult{}, errors.New("app: controller cannot resume adopted Workflow")
		}
		completion, err = controller.WaitResumed(ctx, name, intent.runID, destination, events, retained, *intent.adoption)
	} else {
		completion, err = jobs.controller.Wait(ctx, name, intent.runID, destination)
	}
	var reobserve func(context.Context) (service.JobResult, error)
	if intent.adoption != nil {
		if reader, ok := jobs.controller.(interface {
			TerminalStateResumed(context.Context, string, string, argojob.Adoption, []runprogress.Event) (*argojob.Completion, error)
		}); ok {
			binding := *intent.adoption
			retainedEvents := append([]runprogress.Event(nil), events...)
			// A completed observation may have found a real exit after the seed
			// journal was read. Keep that evidence if a later fallback snapshot
			// loses the exit field; a log/index error must not erase a test failure.
			for _, step := range completion.Steps {
				if step.ExitCodeKnown && step.ExitCode > 0 && step.Status == runprogress.StepFailed && step.Attempt == "" && !step.AttemptAmbiguous {
					retainedEvents = append(retainedEvents, runprogress.Event{Version: runprogress.Version, Stage: step.Stage, Step: step.Step, Ordinal: step.Ordinal, Status: step.Status, ExitCode: step.ExitCode, StartedAt: step.StartedAt, FinishedAt: step.FinishedAt})
				}
			}
			for i := range retainedEvents {
				if value := retainedEvents[i].StartedAt; value != nil {
					copy := *value
					retainedEvents[i].StartedAt = &copy
				}
				if value := retainedEvents[i].FinishedAt; value != nil {
					copy := *value
					retainedEvents[i].FinishedAt = &copy
				}
			}
			reobserve = func(ctx context.Context) (service.JobResult, error) {
				fresh, err := reader.TerminalStateResumed(ctx, name, intent.runID, binding, retainedEvents)
				if errors.Is(err, argojob.ErrNotTerminal) || (err == nil && fresh == nil) {
					return service.JobResult{}, service.ErrJobNotTerminal
				}
				if err != nil {
					return service.JobResult{}, err
				}
				// A weaker later cut snapshot cannot erase a terminal red verdict
				// already observed for this binding (including conflicting exits).
				// A newly observed real failure still replaces an earlier cut.
				if fresh.Interrupted && completion.Terminal && !completion.Succeeded && !completion.Infrastructure && !completion.Interrupted {
					return jobs.result(completion), nil
				}
				return jobs.result(*fresh), nil
			}
		}
	}
	if err != nil && completion.Name == "" {
		return service.JobResult{Reobserve: reobserve}, err
	}
	// A terminal Workflow keeps its outputs even when its observation ended in
	// error (log budget breach, executor identity retirement). Partial evidence
	// from a running Workflow must not trigger live artifact collection.
	if completion.Terminal || completion.Succeeded {
		if reason := jobs.collectArtifacts(context.WithoutCancel(ctx), name, intent.runID, intent.trigger); reason != "" {
			jobs.recordArtifactFailure(context.WithoutCancel(ctx), intent.runID, reason)
		}
	}
	result := jobs.result(completion)
	result.Reobserve = reobserve
	return result, err
}

func (jobs *ArgoJobs) Delete(ctx context.Context, name, runID string) error {
	if jobs.routes != nil {
		child, err := jobs.routes.forName(ctx, name, runID)
		if err != nil {
			return err
		}
		if child.targetBinding == nil {
			return child.Delete(ctx, name, runID)
		}
		if err := jobs.routes.store.CancelExecutionTarget(ctx, runID); err != nil {
			return err
		}
		child.mu.Lock()
		if intent, exists := child.intents[name]; exists {
			intent.cancelled = true
			child.intents[name] = intent
		}
		child.mu.Unlock()
		done, err := child.targetClients.Settle(ctx, time.Time{})
		if err != nil {
			return err
		}
		if !done {
			return argojob.ErrTargetUncertain
		}
		return jobs.routes.completeCleanup(ctx, runID)
	}

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

// SettleCreate bypasses the in-flight Delete shortcut: an ambiguous POST may
// still commit even after an earlier Cancel returned NotFound.
func (jobs *ArgoJobs) SettleCreate(ctx context.Context, name, runID string, notBefore time.Time) (bool, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forName(ctx, name, runID)
		if err != nil {
			return false, err
		}
		if child.targetBinding == nil {
			return child.SettleCreate(ctx, name, runID, notBefore)
		}
		if err := jobs.routes.store.CancelExecutionTarget(ctx, runID); err != nil {
			return false, err
		}
		done, err := child.targetClients.Settle(ctx, notBefore)
		if err != nil || !done {
			return false, err
		}
		if err := jobs.routes.completeCleanup(ctx, runID); err != nil {
			return false, err
		}
		child.forget(name, runID)
		return true, nil
	}

	jobs.mu.Lock()
	intent, exists := jobs.intents[name]
	jobs.mu.Unlock()
	if exists && intent.runID != runID {
		return false, errors.New("app: pending Workflow belongs to a different in-process run")
	}
	controller, ok := jobs.controller.(interface {
		SettleCreate(context.Context, string, string, time.Time) (bool, error)
	})
	if !ok {
		return false, errors.New("app: controller cannot settle uncertain Workflow creates")
	}
	done, err := controller.SettleCreate(ctx, name, runID, notBefore)
	if err != nil || !done {
		return false, err
	}
	jobs.forget(name, runID)
	return true, nil
}

// TerminalResult reconstructs a run's outcome from the Workflow object alone,
// so a server that restarted mid-run records what really happened.
func (jobs *ArgoJobs) TerminalResult(ctx context.Context, name string) (service.JobResult, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forName(ctx, name, "")
		if err != nil {
			return service.JobResult{}, err
		}
		if child.targetClients != nil {
			if err := child.targetClients.VerifyResultCustody(ctx); err != nil {
				return service.JobResult{}, err
			}
		}
		return child.TerminalResult(ctx, name)
	}
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
	if jobs.routes != nil {
		child, err := jobs.routes.forName(ctx, name, "")
		if err != nil {
			return false, err
		}
		return child.Owns(ctx, name)
	}
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
	for _, step := range completion.ReplayAttempts {
		if step.StartedAt != nil || (step.Status != runprogress.StepQueued && step.Status != runprogress.StepSkipped) {
			result.ObservedStepStart = true
		}
	}
	if completion.ObservationComplete {
		observation := &model.RunTerminalObservation{Version: 1, WorkflowName: completion.Name, Nodes: completion.ObservedNodes}
		for _, step := range completion.ReplayAttempts {
			started := step.StartedAt != nil || step.Status == runprogress.StepRunning || step.Status == runprogress.StepPassed || step.Status == runprogress.StepFailed || step.Status == runprogress.StepTimedOut
			observation.Steps = append(observation.Steps, model.RunObservedStep{Stage: step.Stage, Step: step.Step, Status: model.StepStatus(step.Status), Started: started,
				Evicted:         !step.AttemptAmbiguous && (step.InfrastructureReason == "Evicted" || strings.HasPrefix(step.InfrastructureReason, "Evicted: ")),
				StartupDeadline: !step.AttemptAmbiguous && model.IsStartupDeadlineReason(step.InfrastructureReason), StartedAt: step.StartedAt, FinishedAt: step.FinishedAt})
		}
		result.TerminalObservation = observation
	}
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
		// A terminal Workflow can still carry unfinished child nodes (for
		// example after a controller error or stop). Their progress remains
		// in the journal; the result store accepts only observed outcomes.
		if step.Status == runprogress.StepQueued || step.Status == runprogress.StepRunning {
			if result.Status == model.RunPassed {
				result.Status, result.Phase = model.RunFailed, "job"
				result.Error = fmt.Sprintf("terminal Workflow has unfinished step %s/%s", step.Stage, step.Step)
			}
			continue
		}
		result.Steps = append(result.Steps, model.StepResult{
			Stage: step.Stage, Step: step.Step, Ordinal: step.Ordinal, Termination: step.Termination,
			Status: model.StepStatus(step.Status), ExitCode: step.ExitCode,
			StartedAt: step.StartedAt, FinishedAt: step.FinishedAt,
			MaxRSSBytes:     step.Usage.MaxRSSBytes,
			MemoryPeakBytes: step.Usage.MemoryPeakBytes,
			UserCPU:         step.Usage.UserCPU,
			SystemCPU:       step.Usage.SystemCPU,
		})
	}
	if completion.FailedStep != "" {
		result.FailedStage, result.FailedStep = completion.FailedStage, completion.FailedStep
		result.Phase = completion.FailedStage
	}
	if result.Status == model.RunPassed && result.FailedStep != "" {
		result.Status = model.RunFailed
		result.Phase = result.FailedStage
		result.Error = "successful Workflow reported a failed step"
	}
	if result.Status == model.RunFailed && completion.Infrastructure {
		result.Phase = "infrastructure"
		result.FailedStage, result.FailedStep = "", ""
	}
	if completion.Interrupted && completion.Infrastructure && !completion.Succeeded && completion.FailedStep == "" {
		result.Status, result.Phase = model.RunInterrupted, "interrupted"
		result.FailedStage, result.FailedStep = "", ""
	}
	return result
}

// noPipelineError returns a user-facing error when the repository has no
// pipeline configuration file for the given trigger. If the underlying error
// is not os.ErrNotExist it is returned unchanged so callers can use this as a
// pass-through wrapper.
func noPipelineError(err error, trigger pipeline.Trigger, repoName string) error {
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
func (jobs *ArgoJobs) PipelineCredentialed(ctx context.Context, request service.WorkflowRequest) (bool, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forRequest(ctx, request)
		if err != nil {
			return false, err
		}
		return child.PipelineCredentialed(ctx, request)
	}
	source, err := jobs.readRunSource(request)
	if err != nil {
		return false, jobs.noRunPipelineError(err, request)
	}
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		// Decode failure: the workflow YAML is invalid or unreadable. Return
		// the error so the scheduler can log it. The false result is correct:
		// an unparseable pipeline cannot declare secrets, so treating it as
		// non-credentialed is fail-safe (zero secrets, zero publication rights).
		return false, fmt.Errorf("decode pipeline for credential detection: %w", err)
	}
	return workflowCredentialed(workflow)
}

func workflowCredentialed(workflow *wfv1.Workflow) (bool, error) {
	paths, err := argoworkflow.DeclaredSecretPaths(workflow)
	if err != nil {
		return false, err
	}
	credentialed := len(paths) > 0
	// Include inline leaves; use the same predicate as credential admission/plan.
	var walk func(*wfv1.Template, int)
	walk = func(t *wfv1.Template, depth int) {
		if depth > argoworkflow.MaxIdentityWalkDepth {
			credentialed = true
			return
		}
		credentialed = credentialed || argoworkflow.UsesCredentials(t)
		for _, group := range t.Steps {
			for _, step := range group.Steps {
				if step.Inline != nil {
					walk(step.Inline, depth+1)
				}
			}
		}
		if t.DAG != nil {
			for _, task := range t.DAG.Tasks {
				if task.Inline != nil {
					walk(task.Inline, depth+1)
				}
			}
		}
	}
	for i := range workflow.Spec.Templates {
		walk(&workflow.Spec.Templates[i], 0)
	}
	return credentialed, nil
}

var (
	_ service.JobController              = (*ArgoJobs)(nil)
	_ service.JobCreateSettler           = (*ArgoJobs)(nil)
	_ service.JobAdopter                 = (*ArgoJobs)(nil)
	_ service.ReleaseJobController       = (*ArgoJobs)(nil)
	_ service.PipelineSizer              = (*ArgoJobs)(nil)
	_ service.PipelinePlanner            = (*ArgoJobs)(nil)
	_ service.PipelineCredentialDetector = (*ArgoJobs)(nil)
	_ service.PipelineConcurrencyGrouper = (*ArgoJobs)(nil)
	_                                    = time.Second
)

// ObserveScheduling forwards read-only diagnostics without submission or repair.
func (jobs *ArgoJobs) ObserveScheduling(ctx context.Context, run model.Run) (*model.ExecutionObservation, error) {
	if jobs.routes != nil {
		child, err := jobs.routes.forName(ctx, run.WorkflowName, run.ID)
		if err != nil {
			return nil, err
		}
		return child.ObserveScheduling(ctx, run)
	}
	observer, ok := jobs.controller.(interface {
		ObserveScheduling(context.Context, string, string, string) (*model.ExecutionObservation, error)
	})
	if !ok {
		return nil, service.ErrUnavailable
	}
	sha := run.TestedSHA
	if sha == "" {
		sha = run.SHA
	}
	return observer.ObserveScheduling(ctx, run.WorkflowName, run.ID, sha)
}

// SetProfileOutageGate connects the controller profile outage gate to the scheduler.
func (jobs *ArgoJobs) AwaitAdmission(ctx context.Context) error {
	if gate, ok := jobs.controller.(interface{ AwaitAdmission(context.Context) error }); ok {
		return gate.AwaitAdmission(ctx)
	}
	return ctx.Err()
}

func (jobs *ArgoJobs) ControllerProfileStatus() *model.ControllerProfileStatus {
	if source, ok := jobs.controller.(interface {
		ControllerProfileStatus() *model.ControllerProfileStatus
	}); ok {
		return source.ControllerProfileStatus()
	}
	return nil
}

// Assisted continuation (run_hold_* tools) delegates to the native
// controller; a controller without the capability disables the tools.
type assistHoldController interface {
	ListAssistHolds(context.Context, string) ([]model.AssistHold, error)
	ExecAssistHold(context.Context, model.AssistExecRequest) (model.AssistExecResult, error)
	ReleaseAssistHold(context.Context, string) error
	AbandonAssistHold(context.Context, string) error
	EnforceAssistHoldCap(context.Context, map[string]bool) ([]string, error)
}

// AssistPolicy reports the administrator policy Build and the tools share.
func (jobs *ArgoJobs) AssistPolicy() model.AssistPolicy {
	assist := jobs.config.Assist.Effective()
	_, capable := jobs.controller.(assistHoldController)
	return model.AssistPolicy{Enabled: jobs.config.Assist.Enabled && capable, HoldTTL: assist.HoldTTL, MaxConcurrentHolds: assist.MaxConcurrentHolds,
		ExecTimeout: assist.ExecTimeout, ExecOutputBytes: assist.ExecOutputBytes, PatchMaxBytes: assist.PatchMaxBytes, PatchMaxFiles: assist.PatchMaxFiles}
}

func (jobs *ArgoJobs) assistController() (assistHoldController, error) {
	controller, ok := jobs.controller.(assistHoldController)
	if !ok || !jobs.config.Assist.Enabled {
		return nil, errors.New("app: assisted continuation is not available on this engine")
	}
	return controller, nil
}

func (jobs *ArgoJobs) ListAssistHolds(ctx context.Context, workflowName string) ([]model.AssistHold, error) {
	controller, err := jobs.assistController()
	if err != nil {
		return nil, err
	}
	return controller.ListAssistHolds(ctx, workflowName)
}

func (jobs *ArgoJobs) ExecAssistHold(ctx context.Context, request model.AssistExecRequest) (model.AssistExecResult, error) {
	controller, err := jobs.assistController()
	if err != nil {
		return model.AssistExecResult{}, err
	}
	return controller.ExecAssistHold(ctx, request)
}

func (jobs *ArgoJobs) ReleaseAssistHold(ctx context.Context, pod string) error {
	controller, err := jobs.assistController()
	if err != nil {
		return err
	}
	return controller.ReleaseAssistHold(ctx, pod)
}

func (jobs *ArgoJobs) AbandonAssistHold(ctx context.Context, pod string) error {
	controller, err := jobs.assistController()
	if err != nil {
		return err
	}
	return controller.AbandonAssistHold(ctx, pod)
}

// EnforceAssistHoldCap abandons parked attempts above assist.maxConcurrentHolds.
func (jobs *ArgoJobs) EnforceAssistHoldCap(ctx context.Context, released map[string]bool) ([]string, error) {
	controller, err := jobs.assistController()
	if err != nil {
		return nil, nil
	}
	return controller.EnforceAssistHoldCap(ctx, released)
}
