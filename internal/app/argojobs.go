package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argowatch"
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
	Terminate(context.Context, string, string) error
	TerminalState(context.Context, string) (*argojob.Completion, error)
	Owns(context.Context, string) (bool, error)
	Namespace() string
}

// SecretAccessLoader loads approved secret grants for a repository at
// admission time. The store.Store satisfies this interface directly.
// The repository is identified by its durable ID to prevent same-name
// repos under different upstreams from aliasing each other's grants.
type SecretAccessLoader interface {
	ActiveSecretGrants(ctx context.Context, repoID int64, workflow string) (map[string]map[string]bool, error)
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
	watchHub       *argowatch.Hub
	pinnedSources  *PinnedSourceLoader
	sourceRecorder PinnedSourceRecorder

	beforeCreate func(context.Context, service.WorkflowRequest, argojob.Request) error

	capabilityGrants func(context.Context, int64) (*argoworkflow.Capability, error)
	resourceBlobs    workflowpolicy.PolicyGit
	controller       argoControl
	auditor          service.Auditor
	config           argojob.Config
	secretAccess     SecretAccessLoader
	declarations     DeclarationRecorder
	collector        ArtifactCollector
	artifacts        ArtifactStore
	artifactLimit    int64

	// identities holds the live per-repo identity maps. When non-nil,
	// create() snapshots it once per admission so both Builds (audit and
	// controller) bind the same identity version. Updated by the
	// database mutation callback in serve.go.
	identities *argojob.IdentityStore
	// releaseWIF layers audited runtime overrides on the boot release WIF
	// map (#1497). Each admission pins one snapshot for both Builds.
	releaseWIF *argojob.ReleaseWIFRuntime

	mu      sync.Mutex
	intents map[string]argoIntent
}

// argoIntent preserves the submission identity through Stop and terminal observation.
// A Stop before creation records intent; a successful late creation is stopped
// and remains observable by Wait, including its exit handlers.
type argoIntent struct {
	sources   []pipeline.PinnedSource
	adoption  *argojob.Adoption
	runID     string
	trigger   pipeline.Trigger
	created   bool // true once controller.Create has returned successfully
	cancelled bool // true once shutdown has requested cancellation
	terminate bool // immediate native shutdown, preserved across an in-flight Create
}

// ErrCreationCancelled is returned by create() when Stop() cancelled the intent
// while the Workflow was being created. The scheduler records the run as
// cancelled rather than as a creation failure.
var ErrCreationCancelled = errors.New("app: Workflow cancelled before creation")

func NewArgoJobs(controller argoControl, config argojob.Config, auditor service.Auditor, secretAccess SecretAccessLoader) (*ArgoJobs, error) {
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
	jobs := &ArgoJobs{controller: controller, auditor: auditor, config: config, secretAccess: secretAccess, intents: map[string]argoIntent{}}
	return jobs, nil
}

// SetDeclarationRecorder wires the grant declaration recorder after
// construction. When set, create() records (repo, step, path) declarations
// at Workflow admission for `access_list --pending`. Issue #623, finding 4.
func (jobs *ArgoJobs) SetDeclarationRecorder(recorder DeclarationRecorder) {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.declarations = recorder
}

// SetWorkflowCapabilityLoader wires durable operator authority before serving
// requests. The controller independently rechecks it at Kubernetes effects.
func (jobs *ArgoJobs) SetWorkflowCapabilityLoader(load func(context.Context, int64) (*argoworkflow.Capability, error)) {
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
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	jobs.identities = store
}

// SetReleaseWIFRuntime wires the runtime release WIF layer (#1497) before
// serving requests. Without it admissions use the boot map unchanged.
func (jobs *ArgoJobs) SetReleaseWIFRuntime(runtime *argojob.ReleaseWIFRuntime) {
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
	// UNLESS Stop has already marked the intent cancelled, in which case
	// Stop owns the intent lifecycle and this defer must not remove it.
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

	// Early abort: Stop arrived between reservation and seeding.
	jobs.mu.Lock()
	if jobs.intents[request.WorkflowName].cancelled {
		// Delete already marked the intent cancelled and owns cleanup.
		// Remove the tombstone now — no workflow exists to cancel.
		delete(jobs.intents, request.WorkflowName)
		jobs.mu.Unlock()
		return ErrCreationCancelled
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
	kind, err := runKind(request.Run)
	if err != nil {
		return err
	}
	pipelineFile, err := jobs.runSourceFile(request)
	if err != nil {
		return err
	}
	resolved, err := argoworkflow.ResolveRunSource(source, argoworkflow.RunSourceBinding{
		RunID: request.Run.ID, RepositoryID: request.Repository.ID, SHA: testedSHA,
		File: pipelineFile, Trigger: kind, ScheduleEntry: request.Run.ScheduleEntry,
	})
	if err != nil {
		return err
	}
	approvedSecrets, stepGrants, err := jobs.stepSecretAccess(ctx, request.Repository, resolved.Workflow(), pipelineFile)
	if err != nil {
		return err
	}

	submission := argojob.Request{
		SourceReceipt: request.SourceReceipt,
		PipelineFile:  pipelineFile,
		RunID:         request.Run.ID, RepositoryID: request.Repository.ID, Name: request.WorkflowName,
		Repo: request.Repository.Name, UpstreamName: request.UpstreamName, UpstreamOrg: request.UpstreamOrg,
		Ref: request.Run.Ref, SHA: testedSHA, Trigger: trigger, RunTrigger: kind, ScheduleEntry: request.Run.ScheduleEntry, Source: source,
		Assist:    request.Run.AssistRequested,
		SourceDir: request.SourceDir, ApprovedSecrets: approvedSecrets,
		StepGrants: stepGrants,
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
	// Snapshot the identity store once for both Builds of this admission
	// (audit and controller), so the audit record and the pod spec agree
	// on which ServiceAccount the run executes under. (Issue #465)
	if jobs.identities != nil {
		submission.Identities = jobs.identities.Snapshot()
	}
	submission.ReleaseWIF = jobs.releaseWIFSnapshot()
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

	jobs.mu.Lock()
	intent := jobs.intents[request.WorkflowName]
	intent.created = true
	jobs.intents[request.WorkflowName] = intent
	jobs.mu.Unlock()
	if intent.cancelled {
		// A successful create must remain observable through onExit.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		shutdown := jobs.controller.Cancel
		if intent.terminate {
			shutdown = jobs.controller.Terminate
		}
		if err := shutdown(stopCtx, createdName, request.Run.ID); err != nil {
			log.Printf("Workflow %s: Stop deferred to the durable cancellation owner: %v", createdName, err)
		}
	}

	success = true
	return nil
}

// auditSubmission records the exact identity binding this run will execute
// under, before the Workflow object exists.
//
// On this tier Oberth never holds the secret values: secretstore exec fetches them
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
		"sources":                         sourcesForAudit(submission.PinnedSources),
	}
	if submission.SourceReceipt != nil {
		details["source_receipt"] = submission.SourceReceipt.Summary()
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
				Workflow: request.PipelineFile,
				Step:     tmpl.Name,
				Path:     p,
			})
		}
	}
	// Also record paths from the annotation with wildcard step for templates
	// that use declared paths not captured above.
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
				declarations = append(declarations, store.GrantDeclaration{Workflow: request.PipelineFile, Step: "*", Path: p})
			}
		}
	}
	if len(declarations) == 0 {
		return
	}
	for i := range declarations {
		if declarations[i].Workflow == "" {
			declarations[i].Workflow, _ = argoworkflow.TriggerFile(trigger)
		}
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
func (jobs *ArgoJobs) Adopt(ctx context.Context, run model.Run) (bool, error) {
	controller, ok := jobs.controller.(interface {
		Adopt(context.Context, argojob.AdoptionRequest) (argojob.Adoption, bool, error)
	})
	if !ok {
		return false, errors.New("app: controller cannot adopt Workflows")
	}
	trigger, err := runTrigger(run)
	if err != nil {
		return false, err
	}
	kind, err := runKind(run)
	if err != nil {
		return false, err
	}
	sha := run.TestedSHA
	if sha == "" {
		sha = run.SHA
	}
	binding, found, err := controller.Adopt(ctx, argojob.AdoptionRequest{
		PinnedSources: run.SourceBindings, RepositoryID: run.RepoID, Name: run.WorkflowName, RunID: run.ID, Ref: run.Ref, SHA: sha,
		Trigger: trigger, RunTrigger: kind, ScheduleEntry: run.ScheduleEntry,
	})
	if errors.Is(err, argojob.ErrAdoptionRefused) {
		return false, fmt.Errorf("%w: %w", service.ErrAdoptionRefused, err)
	}
	if err != nil || !found {
		return found, err
	}
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	if current, exists := jobs.intents[run.WorkflowName]; exists {
		if current.runID != run.ID || current.cancelled || current.adoption == nil || !reflect.DeepEqual(*current.adoption, binding) {
			return false, fmt.Errorf("%w: app: conflicting Workflow adoption intent", service.ErrAdoptionRefused)
		}
		return true, nil
	}
	jobs.intents[run.WorkflowName] = argoIntent{runID: run.ID, trigger: trigger, created: true, adoption: &binding, sources: run.SourceBindings}
	return true, nil
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
	artifactReason := ""
	if completion.Terminal || completion.Succeeded {
		if reason := jobs.collectArtifacts(context.WithoutCancel(ctx), name, intent.runID, intent.trigger); reason != "" {
			jobs.recordArtifactFailure(context.WithoutCancel(ctx), intent.runID, reason)
			artifactReason = reason
		}
	}
	result := jobs.result(completion)
	if artifactReason == model.ArtifactCollectionDeadlineReason {
		result.Error = model.WithArtifactCollectionDeadline(result.Error)
	}
	result.Reobserve = reobserve
	return result, err
}

func (jobs *ArgoJobs) Stop(ctx context.Context, name, runID string) error {
	return jobs.shutdown(ctx, name, runID, false)
}

func (jobs *ArgoJobs) Terminate(ctx context.Context, name, runID string) error {
	return jobs.shutdown(ctx, name, runID, true)
}

func (jobs *ArgoJobs) shutdown(ctx context.Context, name, runID string, terminate bool) error {
	if strings.TrimSpace(runID) == "" {
		return errors.New("app: durable run ID is required to shut down a Workflow")
	}
	jobs.mu.Lock()
	current, known := jobs.intents[name]
	if known && current.runID != runID {
		jobs.mu.Unlock()
		return fmt.Errorf("app: Workflow %s belongs to a different in-process run", name)
	}
	if known {
		current.cancelled = true
		current.terminate = current.terminate || terminate
		jobs.intents[name] = current
		terminate = current.terminate
	}
	jobs.mu.Unlock()
	shutdown := jobs.controller.Cancel
	if terminate {
		shutdown = jobs.controller.Terminate
	}
	if known && !current.created {
		// Create owns the outcome; a late accepted object inherits this exact mode.
		_ = shutdown(ctx, name, runID)
		return nil
	}
	return shutdown(ctx, name, runID)
}

// SettleCreate bypasses the in-flight Stop shortcut: an ambiguous POST may
// still commit even after an earlier Cancel returned NotFound.
func (jobs *ArgoJobs) SettleCreate(ctx context.Context, name, runID string, notBefore time.Time) (bool, error) {
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
	for _, step := range completion.ReplayAttempts {
		if step.Status != runprogress.StepQueued && step.Status != runprogress.StepSkipped {
			result.ObservedStepStart = true
		}
	}
	if completion.ObservationComplete {
		observation := &model.RunTerminalObservation{Version: 1, WorkflowName: completion.Name, Nodes: completion.ObservedNodes}
		for _, step := range completion.ReplayAttempts {
			started := step.Status == runprogress.StepRunning || step.Status == runprogress.StepPassed || step.Status == runprogress.StepFailed || step.Status == runprogress.StepTimedOut
			observation.Steps = append(observation.Steps, model.RunObservedStep{Stage: step.Stage, Step: step.Step, Status: model.StepStatus(step.Status), Started: started,
				Evicted:         !step.AttemptAmbiguous && (step.InfrastructureReason == "Evicted" || strings.HasPrefix(step.InfrastructureReason, "Evicted: ")),
				NodeReboot:      !step.AttemptAmbiguous && model.IsNodeRebootReason(step.InfrastructureReason),
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

			continue
		}
		result.Steps = append(result.Steps, model.StepResult{
			Stage: step.Stage, Step: step.Step, Ordinal: step.Ordinal, Termination: step.Termination,
			Status: model.StepStatus(step.Status), ExitCode: step.ExitCode,
			StartedAt: step.StartedAt, FinishedAt: step.FinishedAt,
		})
	}
	if !completion.Succeeded && completion.FailedStep != "" {
		result.FailedStage, result.FailedStep = completion.FailedStage, completion.FailedStep
		result.Phase = completion.FailedStage
	}

	if result.Status == model.RunFailed && completion.Infrastructure {
		result.Phase = "infrastructure"
		result.FailedStage, result.FailedStep = "", ""
	}

	return operatorShutdownResult(result, completion)
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

var (
	_ service.JobController        = (*ArgoJobs)(nil)
	_ service.JobCreateSettler     = (*ArgoJobs)(nil)
	_ service.JobAdopter           = (*ArgoJobs)(nil)
	_ service.ReleaseJobController = (*ArgoJobs)(nil)
	_ service.PipelinePlanner      = (*ArgoJobs)(nil)
	_                              = time.Second
)

// ObserveScheduling forwards read-only diagnostics without submission or repair.
func (jobs *ArgoJobs) ObserveScheduling(ctx context.Context, run model.Run) (*model.ExecutionObservation, error) {
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

// ObserveSynchronization forwards the read-only native lock observation (#1514).
func (jobs *ArgoJobs) ObserveSynchronization(ctx context.Context, run model.Run) (*model.SyncObservation, error) {
	observer, ok := jobs.controller.(interface {
		ObserveSynchronization(context.Context, string, string) (*model.SyncObservation, error)
	})
	if !ok {
		return nil, service.ErrUnavailable
	}
	return observer.ObserveSynchronization(ctx, run.WorkflowName, run.ID)
}

// Assisted continuation (run_hold_* tools) delegates to the native
// controller; a controller without the capability disables the tools.
type assistHoldController interface {
	ListAssistHolds(context.Context, string) ([]model.AssistHold, error)
	ExecAssistHold(context.Context, model.AssistExecRequest) (model.AssistExecResult, error)
	ReleaseAssistHold(context.Context, string) error
	AbandonAssistHold(context.Context, string) error
	EnforceAssistHolds(context.Context, map[string]model.AssistPodActivity, model.AssistActivityRecheck, time.Time) ([]model.AssistAbandon, error)
}

// AssistPolicy reports the administrator policy Build and the tools share.
func (jobs *ArgoJobs) AssistPolicy() model.AssistPolicy {
	assist := jobs.config.Assist.Effective()
	_, capable := jobs.controller.(assistHoldController)
	return model.AssistPolicy{Enabled: jobs.config.Assist.Enabled && capable, HoldTTL: assist.HoldTTL, MaxConcurrentHolds: assist.MaxConcurrentHolds,
		ExecTimeout: assist.ExecTimeout, ExecOutputBytes: assist.ExecOutputBytes, PatchMaxBytes: assist.PatchMaxBytes, PatchMaxFiles: assist.PatchMaxFiles,
		ScratchBytes: max(assist.ScratchBytes, 0)}
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

// EnforceAssistHolds abandons parked attempts older than assist.holdTTL and
// above assist.maxConcurrentHolds; released holds are skipped.
func (jobs *ArgoJobs) EnforceAssistHolds(ctx context.Context, activity map[string]model.AssistPodActivity, recheck model.AssistActivityRecheck, now time.Time) ([]model.AssistAbandon, error) {
	controller, err := jobs.assistController()
	if err != nil {
		return nil, nil
	}
	return controller.EnforceAssistHolds(ctx, activity, recheck, now)
}

// SetWatchHub supplies the server-owned namespace cache before serving.
func (jobs *ArgoJobs) SetWatchHub(hub *argowatch.Hub) { jobs.watchHub = hub }

// RunWatch ties the informers to the server lifetime.
func (jobs *ArgoJobs) RunWatch(ctx context.Context) {
	if jobs.watchHub != nil {
		jobs.watchHub.Run(ctx)
	}
}

// WaitForWatchSync gates recovery and dispatch, but never the API or Git.
func (jobs *ArgoJobs) WaitForWatchSync(ctx context.Context) error {
	if jobs.watchHub == nil {
		return errors.New("app: Workflow watch hub is required")
	}
	for {
		attempt, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := jobs.watchHub.WaitForSync(attempt)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Print("WARN: waiting for Workflow and Pod watch caches to sync")
	}
}
