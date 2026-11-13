package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// WorkflowClient is the narrow slice of the generated Argo clientset this
// engine uses. Narrowing it keeps the package testable with a fake and makes
// the engine's whole Kubernetes surface reviewable in one place: create, read,
// delete. It never updates a Workflow, so it can never rewrite a running one's
// spec.
type WorkflowClient interface {
	Create(ctx context.Context, workflow *wfv1.Workflow, options metav1.CreateOptions) (*wfv1.Workflow, error)
	Get(ctx context.Context, name string, options metav1.GetOptions) (*wfv1.Workflow, error)
	Delete(ctx context.Context, name string, options metav1.DeleteOptions) error
}

const (
	pollInterval        = 500 * time.Millisecond
	deletionGracePeriod = int64(10)
	// mainContainerName is the container Argo runs the template's command in.
	mainContainerName = "main"
	// workflowReadGrace bounds how long Wait keeps polling through failed
	// Workflow reads (API server restart, connection reset) before it gives
	// up on observing the run.
	workflowReadGrace = 2 * time.Minute
)

// Process initialization is the conservative boundary for restart attribution.
// A step first started later cannot have been cut by this server restart.
var serverStartedAt = time.Now().UTC()

// ErrNotTerminal reports that a Workflow has not reached a terminal phase.
var ErrNotTerminal = errors.New("argojob: Workflow has not reached a terminal state")

// Controller submits Workflows and supervises them to a terminal result.
type Controller struct {
	containerLogs      runlog.ContainerLogStore
	containerLogSource PodLogStream
	containerLogPod    func(context.Context, string, string) (*corev1.Pod, error)
	capabilities       func(context.Context, int64) (*argoworkflow.Capability, error)
	restartedAt        time.Time
	timings            TimingRecorder
	nodeBoots          NodeBootRecorder
	hub                ObservationHub
	workflows          WorkflowClient
	kube               kubernetes.Interface
	seeder             *SourceSeeder
	config             Config
	interval           time.Duration
	readGrace          time.Duration
	// assistSeen remembers the holds the sweep already logged.
	assistSeen struct {
		sync.Mutex
		pods map[string]bool
	}
}

// NewController builds the engine. client may be nil when logs are supplied
// directly (tests); in production it is the same in-cluster Kubernetes client
// the Job engine uses, because step logs are ordinary pod logs.
func NewController(workflows WorkflowClient, client kubernetes.Interface, config Config) (*Controller, error) {
	if workflows == nil {
		return nil, errors.New("argojob: an Argo Workflow client is required")
	}
	config.applyDefaults()
	config.ReleaseWIF = config.ReleaseWIF.Clone()
	config.ReleaseTagRoutes = config.ReleaseTagRoutes.Clone()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	controller := &Controller{restartedAt: serverStartedAt, workflows: workflows, kube: client, config: config, interval: pollInterval, readGrace: workflowReadGrace}
	if client != nil {
		controller.containerLogPod = func(ctx context.Context, namespace, pod string) (*corev1.Pod, error) {
			return client.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
		}
		controller.containerLogSource = func(ctx context.Context, namespace, pod string, options corev1.PodLogOptions) (io.ReadCloser, error) {
			return client.CoreV1().Pods(namespace).GetLogs(pod, &options).Stream(ctx)
		}

	}
	return controller, nil
}

// WithSourceSeeder supplies the component that copies a run's checkout into a
// claim in the pipeline namespace. Without it Create has no source volume to
// give a Workflow, and refuses to submit one rather than starting a pipeline
// that would fail on its first step with an empty working directory.
func (controller *Controller) WithSourceSeeder(seeder *SourceSeeder) *Controller {
	controller.seeder = seeder
	if seeder != nil {
		seeder.hub = controller.hub
		seeder.workflows = controller.workflows
	}
	return controller
}

// Namespace reports where this engine creates Workflows.
func (controller *Controller) Namespace() string { return controller.config.Namespace }

// Create submits one run's Workflow. Submission is idempotent against an
// identical object: a retry after an ambiguous create adopts the existing
// Workflow when it belongs to the same run and carries the same spec digest,
// and refuses otherwise rather than executing something that was not built
// from this run's reviewed source.
func (controller *Controller) Create(ctx context.Context, request Request) (name string, resultErr error) {
	if err := controller.verifyCapabilityGrant(ctx, request.RepositoryID, request.Capability); err != nil {
		return "", err
	}

	if controller.seeder == nil {
		return "", errors.New("argojob: no source seeder is configured; " +
			"a pipeline cannot mount the server's own claim across namespaces")
	}
	// Seed store assets only for admitted consumers, using Build's grant rule.
	needsSecretStore := requestNeedsSecretStore(request)

	// GET the Workflow BEFORE any seeding. If the same submission already
	// exists, return/repair ownership without touching its claim. This
	// prevents a retry or name collision from rewriting a running Workflow's
	// source in place (finding 2, issue #29).
	existing, getErr := controller.workflows.Get(ctx, request.Name, metav1.GetOptions{})
	if getErr == nil {
		if err := validateHelperStorage(existing); err != nil {
			return "", err
		}
		request.SourceVolume = controller.expectedSourceVolume(request)
		// Reconstruct the current delivery footprint without reseeding.
		// Unsupported helper policies have already been refused.
		intended, buildErr := Build(controller.config, request)
		if buildErr != nil {
			return "", buildErr
		}
		if subErr := sameSubmission(existing, intended); subErr != nil {
			return "", subErr
		}
		// Same submission exists; repair claim ownership if needed.
		claimName := sourceClaimName(request.Name)
		controller.adoptClaimBestEffort(ctx, claimName, existing.Name, string(existing.UID))
		controller.retainWorkflowTiming(ctx, existing)
		return existing.Name, nil
	}
	if !apierrors.IsNotFound(getErr) {
		// Non-definitive GET error: cannot tell if the Workflow exists.
		// Do not seed — a retry will re-check.
		return "", fmt.Errorf("argojob: pre-check Workflow %s: %w", request.Name, getErr)
	}

	// Workflow definitively does not exist. Seed the source claim.
	controller.timingMilestone(ctx, request.RunID, "source-seed-started")
	volume, err := controller.seeder.SeedRequest(ctx, request, needsSecretStore)
	if err != nil {
		return "", &beforeWorkflowCreateFailure{cause: err}
	}
	controller.timingMilestone(ctx, request.RunID, "source-ready")
	request.SourceVolume = volume

	intended, err := Build(controller.config, request)
	if err != nil {
		controller.seeder.DeleteClaim(context.WithoutCancel(ctx), volume.ClaimName)
		return "", err
	}
	// Fail fast when the built Workflow's ServiceAccount does not exist in the
	// pipeline namespace. Without this, a missing per-repo SA (issue #272) only
	// surfaces as a Kubernetes "forbidden: error looking up service account"
	// after the Workflow is created, with no actionable message.
	if saErr := controller.verifyServiceAccount(ctx, intended.Namespace, intended.Spec.ServiceAccountName); saErr != nil {
		controller.seeder.DeleteClaim(context.WithoutCancel(ctx), volume.ClaimName)
		return "", saErr
	}
	// Verify per-step SAs exist in the pipeline namespace (issue #623,
	// finding 2). Templates with per-step identities have their own
	// ServiceAccountName set by applyPerStepIdentities; a missing SA here
	// means `oberth install --install-secretstore --upgrade` has not been
	// run since the grant was created.
	if saErr := controller.verifyPerStepServiceAccounts(ctx, intended); saErr != nil {
		controller.seeder.DeleteClaim(context.WithoutCancel(ctx), volume.ClaimName)
		return "", saErr
	}

	created, rejected, createErr := controller.createWorkflowWithRetry(ctx, request, intended)
	if createErr != nil {
		if rejected {
			controller.seeder.DeleteClaim(context.WithoutCancel(ctx), volume.ClaimName)
		}
		return "", createErr
	}
	controller.adoptClaimBestEffort(ctx, volume.ClaimName, created.Name, string(created.UID))
	controller.retainWorkflowTiming(ctx, created)
	return created.Name, nil
}

// adoptClaimBestEffort binds the claim's lifetime to the Workflow's so
// deleting the Workflow collects the storage. A failure leaves an unowned
// claim for SweepOrphanedSourceClaims rather than breaking a live run. Retry
// once so a transient ownership write cannot prevent later artifact collection.
func (controller *Controller) adoptClaimBestEffort(ctx context.Context, claimName, workflowName, workflowUID string) {
	for range 2 {
		if controller.seeder.AdoptClaim(ctx, claimName, workflowName, workflowUID) == nil {
			return
		}
	}
}

// verifyServiceAccount checks that a per-repo ServiceAccount exists in the
// pipeline namespace before the Workflow is created. Shared SAs (pipeline,
// credentialed, ci-secrets, executor) are chart-managed and always present;
// per-repo SAs are selected dynamically but only materialized by install
// --upgrade --install-secretstore, so they can be missing for newly granted
// repos (issue #272). Without this check, the failure manifests only as an
// opaque "forbidden: error looking up service account" from the kubelet after
// the Workflow is created, with no actionable message.
func (controller *Controller) verifyServiceAccount(ctx context.Context, namespace, name string) error {
	if controller.kube == nil || name == "" {
		return nil
	}
	// Only verify per-repo SAs. Shared SAs are chart-managed and do not
	// suffer the materialization gap this check targets.
	if controller.isSharedServiceAccount(name) {
		return nil
	}
	_, err := controller.kube.CoreV1().ServiceAccounts(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("argojob: ServiceAccount %q does not exist in namespace %q — "+
				"run `oberth install --upgrade --install-secretstore` to materialize per-repo identities "+
				"for newly granted repositories", name, namespace)
		}
		return fmt.Errorf("argojob: verify ServiceAccount %q in %q: %w", name, namespace, err)
	}
	return nil
}

// verifyPerStepServiceAccounts checks that every per-step SA assigned to a
// template by applyPerStepIdentities exists in the pipeline namespace. A
// template-level SA that differs from the workflow-level SA and is not one of
// the shared SAs is a per-step (or per-repo) SA that may not have been
// materialized yet. Issue #623, finding 2.
func (controller *Controller) verifyPerStepServiceAccounts(ctx context.Context, workflow *wfv1.Workflow) error {
	if controller.kube == nil {
		return nil
	}
	workflowSA := workflow.Spec.ServiceAccountName
	checked := make(map[string]bool)
	for _, tmpl := range workflow.Spec.Templates {
		sa := tmpl.ServiceAccountName
		if sa == "" || sa == workflowSA {
			continue
		}
		if checked[sa] {
			continue
		}
		checked[sa] = true
		if controller.isSharedServiceAccount(sa) {
			continue
		}
		_, err := controller.kube.CoreV1().ServiceAccounts(workflow.Namespace).Get(ctx, sa, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				if controller.isReleaseWIFStepServiceAccount(sa) {
					return fmt.Errorf("argojob: WIF-only ServiceAccount %q does not exist in namespace %q; apply the administrator argo.releaseWIF configuration before retrying", sa, workflow.Namespace)
				}
				return fmt.Errorf("argojob: per-step ServiceAccount %q does not exist in namespace %q — "+
					"run `oberth secretstore sync` and `oberth install --install-secretstore --upgrade` "+
					"to materialize per-step identities for named grants",
					sa, workflow.Namespace)
			}
			return fmt.Errorf("argojob: verify per-step ServiceAccount %q in %q: %w", sa, workflow.Namespace, err)
		}
	}
	return nil
}

// isSharedServiceAccount returns true when the name matches one of the four
// chart-managed shared identities. These are always present and do not need
// the per-repo materialization check.
func (controller *Controller) isSharedServiceAccount(name string) bool {
	return name == controller.config.PipelineServiceAccount ||
		name == controller.config.CredentialedServiceAccount ||
		name == controller.config.CISecretsServiceAccount ||
		name == controller.config.ExecutorServiceAccount
}

// An owned but altered Workflow must be cleaned up before releasing its grant.
// The named classification crosses the engine boundary without exposing a spec.
type workflowSpecMismatch struct{}

func (*workflowSpecMismatch) Error() string {
	return "argojob: Workflow spec mismatch: verified policy changed"
}
func (*workflowSpecMismatch) AmbiguousCreate() bool      { return true }
func (*workflowSpecMismatch) WorkflowSpecMismatch() bool { return true }

func sameSubmission(existing, intended *wfv1.Workflow) error {
	// Establish the complete submission identity before classifying a mismatch
	// as ours to clean up. A foreign run/source never enters cancellation.
	existingRun, existingIdentity, ok := WorkflowMeta(existing)
	if !ok {
		return fmt.Errorf("argojob: Workflow %s already exists and is not an Oberth run", intended.Name)
	}
	intendedRun, intendedIdentity, _ := WorkflowMeta(intended)
	if existingRun != intendedRun {
		return fmt.Errorf("argojob: Workflow %s belongs to run %s, not %s", intended.Name, existingRun, intendedRun)
	}
	if existingIdentity != intendedIdentity {
		return fmt.Errorf("argojob: Workflow %s exists with a different spec than this run's source produces", intended.Name)
	}
	sources, err := argoworkflow.PinnedSources(intended)
	if err != nil {
		return err
	}
	if err := verifyDeliveredPinnedSources(existing, sources); err != nil {
		return &workflowSpecMismatch{}
	}
	if intended.Annotations[argoworkflow.NonrootTemplatesAnnotation] != "" && !apiequality.Semantic.DeepEqual(existing.Spec, intended.Spec) {
		return &workflowSpecMismatch{}
	}
	// Ordinary pipelines retain the annotation-only identity check. Namespace
	// Workflow-create RBAC remains part of that trust boundary.
	return nil
}

// Wait observes a submitted Workflow to its terminal phase. Container log
// copies are retained separately from the observation output.
//
// The server derives progress events from the Workflow node tree and writes
// the retained marker protocol consumed by the scheduler, index and dashboard.
func (controller *Controller) Wait(ctx context.Context, name, runID string, destination io.Writer) (Completion, error) {
	return controller.wait(ctx, name, runID, destination, nil, 0, Adoption{})
}

// WaitResumed restores recorded progress for an adopted Workflow.
func (controller *Controller) WaitResumed(ctx context.Context, name, runID string, destination io.Writer, events []runprogress.Event, retained int64, binding Adoption) (Completion, error) {
	if binding.UID == "" {
		binding.RestartedAt = time.Time{}
	}
	return controller.wait(ctx, name, runID, destination, events, retained, binding)
}

func (controller *Controller) wait(ctx context.Context, name, runID string, destination io.Writer, events []runprogress.Event, retained int64, binding Adoption) (result Completion, retErr error) {
	if retained < 0 {
		return Completion{}, errors.New("argojob: invalid retained log size")
	}
	reporter := newProgressReporter(destination)
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return Completion{}, err
		}
		reporter.reported[event.Stage+"/"+event.Step] = event.Status
		reporter.ordinals[event.Stage+"/"+event.Step] = event.Ordinal
		reporter.ordinalOwners[event.Ordinal] = event.Stage + "/" + event.Step
	}
	ticker := time.NewTicker(controller.interval)
	defer ticker.Stop()
	delivered := map[types.UID]bool{}
	var admissionWatch executorIdentityProgress
	var readFailingSince time.Time
	timed := map[string]bool{}
	boundTiming := false
	for {
		readCtx, cancelRead := context.WithTimeout(ctx, executorIdentityAttemptTimeout)
		workflow, err := controller.workflows.Get(readCtx, name, metav1.GetOptions{})
		cancelRead()
		if err != nil {
			if apierrors.IsNotFound(err) {
				return Completion{}, fmt.Errorf("argojob: Workflow %s disappeared before completing", name)
			}
			// One failed read must not end a run whose Workflow is still
			// progressing: keep polling for a bounded grace and report only
			// a persistent outage.
			if readFailingSince.IsZero() {
				readFailingSince = time.Now()
			}
			if ctx.Err() != nil || time.Since(readFailingSince) >= controller.readGrace {
				return Completion{}, fmt.Errorf("argojob: read Workflow %s: %w", name, err)
			}
			select {
			case <-ctx.Done():
				return Completion{}, ctx.Err()
			case <-ticker.C:
			}
			continue
		}
		readFailingSince = time.Time{}
		if err := controller.verifyOwnership(workflow, runID); err != nil {
			return Completion{}, err
		}
		if binding.UID != "" && !binding.matches(workflow) {
			return Completion{}, errors.New("argojob: adopted Workflow identity changed")
		}
		// Observe already-created Pods before identity delivery can block on a
		// later Pod: early step output must not wait behind that setup RPC.
		if !workflow.Status.Phase.Completed() {
			controller.observeRunningNodeBoots(ctx, workflow)
		}
		if err := controller.checkExecutorIdentityProgress(ctx, workflow, &admissionWatch, delivered); err != nil {
			return completionFor(workflow), err
		}
		// Bound reads and identity transport even when no waiter observation exists.
		deliveryCtx, stopDelivery := context.WithTimeout(ctx, identityObservationTimeout)
		identityReadUnavailable := false
		deliveryErr := controller.deliverExecutorTokensObserved(deliveryCtx, workflow, delivered, func() { identityReadUnavailable = true })
		if deliveryCtx.Err() != nil && ctx.Err() == nil {
			deliveryErr = &executorIdentityProgressError{reason: "Kubernetes executor identity observation unavailable for 2m"}
		}
		stopDelivery()
		if deliveryErr == nil {
			deliveryErr = admissionWatch.observeIdentityReads(identityReadUnavailable, time.Now())
		}
		if err := deliveryErr; err != nil {
			// Preserve observed execution evidence; a delivery error on a later
			// Pod must not make already executed steps look like a pre-step failure.
			return completionFor(workflow), err
		}
		if workflow.Status.Phase.Completed() {
			// One final read after the controller marks completion: the last
			// node transitions and the workflow phase are not written
			// atomically, so a status read that races the final update can
			// miss a step's terminal event.
			settled, completeRead := controller.settle(ctx, name, runID, workflow)
			if binding.UID != "" && !binding.matches(settled) {
				return Completion{}, errors.New("argojob: adopted Workflow identity changed at completion")
			}
			controller.retainWorkflowTiming(ctx, settled)
			completion := completionFor(settled)
			completion.ObservationComplete = completion.ObservationComplete && completeRead
			completion.Steps = reporter.order(completion.Steps)
			preserveObservedExitCodes(&completion, workflow)
			preserveRecordedExitCodes(&completion, events)
			controller.collectRestartStepDiagnostics(ctx, settled, &completion, binding.RestartedAt)
			controller.observeStepProgress(ctx, settled, reporter, destination)
			controller.captureObservedContainerLogs(ctx, settled, runID)
			completion.classifyRestartCut()
			reporter.finish(destination)
			if err := controller.retireExecutorIdentity(ctx, settled); err != nil {
				return completion, fmt.Errorf("retire executor identity: %w", err)
			}
			if progressErr := reporter.failure(); progressErr != nil {
				return completion, fmt.Errorf("argojob: run progress markers degraded: %w", progressErr)
			}
			return completion, nil
		}
		if controller.timings != nil {
			if !boundTiming {
				controller.retainWorkflowTiming(ctx, workflow)
				boundTiming = true
			}
			observation := Completion{}
			for _, step := range reporter.order(StepResults(workflow)) {
				if terminal(step.Status) && !timed[step.NodeID] {
					observation.Steps = append(observation.Steps, step)
				}
			}
			if len(observation.Steps) > 0 {
				controller.collectStepDiagnostics(ctx, workflow, &observation)
				for _, step := range observation.Steps {
					timed[step.NodeID] = true
				}
			}
		}
		controller.observeStepProgress(ctx, workflow, reporter, destination)
		controller.captureObservedContainerLogs(ctx, workflow, runID)

		select {
		case <-ctx.Done():
			return Completion{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Status observation remains independent of native container bytes.
func (controller *Controller) observeStepProgress(ctx context.Context, workflow *wfv1.Workflow, reporter *progressReporter, destination io.Writer) {
	observed := controller.observedStepAttempts(ctx, workflow)
	for _, step := range observed {
		if step.Superseded && terminal(step.Status) {
			reporter.observe(destination, step)
		}
	}
	for _, step := range finalStepResults(observed) {
		reporter.observe(destination, step)
	}
}

// settle re-reads the Workflow once after it reports completion, preferring
// the later observation when it carries more terminal node results.
func (controller *Controller) settle(ctx context.Context, name, runID string, observed *wfv1.Workflow) (*wfv1.Workflow, bool) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	final, err := controller.workflows.Get(settleCtx, name, metav1.GetOptions{})
	if err != nil || controller.verifyOwnership(final, runID) != nil {
		return observed, false
	}
	if len(final.Status.Nodes) < len(observed.Status.Nodes) {
		return observed, false
	}
	return final, true
}

// verifyOwnership refuses to report on a Workflow that is not this run's. A
// name collision with a foreign object must never become a run result.
func (controller *Controller) verifyOwnership(workflow *wfv1.Workflow, runID string) error {
	observed, _, ok := WorkflowMeta(workflow)
	if !ok {
		return fmt.Errorf("argojob: Workflow %s is not an Oberth run", workflow.Name)
	}
	if runID != "" && observed != runID {
		return fmt.Errorf("argojob: Workflow %s belongs to run %s, not %s", workflow.Name, observed, runID)
	}
	return nil
}

func (controller *Controller) TerminalState(ctx context.Context, name string) (*Completion, error) {
	return controller.terminalState(ctx, name, "", Adoption{}, nil)
}

// TerminalStateResumed keeps the verified adoption binding on the fallback read
// after a terminal Wait error, such as lost logs or executor identity retirement.
func (controller *Controller) TerminalStateResumed(ctx context.Context, name, runID string, binding Adoption, events []runprogress.Event) (*Completion, error) {
	if binding.UID == "" {
		return nil, errors.New("argojob: missing resumed Workflow binding")
	}
	return controller.terminalState(ctx, name, runID, binding, events)
}

func (controller *Controller) terminalState(ctx context.Context, name, wantedRun string, binding Adoption, events []runprogress.Event) (*Completion, error) {
	workflow, err := controller.workflows.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrNotTerminal
	}
	if err != nil {
		return nil, fmt.Errorf("argojob: read Workflow %s: %w", name, err)
	}
	runID, _, ok := WorkflowMeta(workflow)
	if !ok {
		return nil, ErrNotTerminal
	}
	if wantedRun != "" && runID != wantedRun {
		return nil, errors.New("argojob: resumed Workflow belongs to another run")
	}
	if binding.UID != "" && !binding.matches(workflow) {
		return nil, errors.New("argojob: resumed Workflow identity changed")
	}
	if !workflow.Status.Phase.Completed() {
		return nil, ErrNotTerminal
	}
	settled, completeRead := controller.settle(ctx, name, runID, workflow)
	if binding.UID != "" && !binding.matches(settled) {
		return nil, errors.New("argojob: resumed Workflow identity changed at completion")
	}
	completion := completionFor(settled)
	completion.ObservationComplete = completion.ObservationComplete && completeRead
	controller.retainWorkflowTiming(ctx, settled)
	preserveObservedExitCodes(&completion, workflow)
	preserveRecordedExitCodes(&completion, events)
	controller.collectRestartStepDiagnostics(ctx, settled, &completion, binding.RestartedAt)
	completion.classifyRestartCut()
	return &completion, nil
}

// Owns reports whether a Workflow with this name exists and belongs to Oberth.
// The dispatcher uses it to route a name back to its engine after a restart,
// when no in-process routing table survives.
func (controller *Controller) Owns(ctx context.Context, name string) (bool, error) {
	if controller.hub != nil && controller.hub.Healthy() {
		if workflow, ok := controller.hub.Workflow(name); ok {
			_, _, owned := WorkflowMeta(workflow)
			return owned, nil
		}
	}
	workflow, err := controller.workflows.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("argojob: read Workflow %s: %w", name, err)
	}
	_, _, ok := WorkflowMeta(workflow)
	return ok, nil
}

// Cancel deletes a run's Workflow. Deleting the object cascades to its pods
// through Argo's own ownership references. A Workflow that belongs to another
// run is left alone and reported, never deleted.
func (controller *Controller) Cancel(ctx context.Context, name, runID string) error {
	workflow, err := controller.workflows.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("argojob: read Workflow %s before cancelling: %w", name, err)
	}
	if err := controller.verifyOwnership(workflow, runID); err != nil {
		return err
	}
	grace := deletionGracePeriod
	propagation := metav1.DeletePropagationBackground
	err = controller.workflows.Delete(ctx, name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace,
		PropagationPolicy:  &propagation,
		Preconditions:      &metav1.Preconditions{UID: &workflow.UID},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return fmt.Errorf("argojob: delete Workflow %s: %w", name, err)
	}
	return nil
}

// progressReporter turns repeated status observations into the monotonic
// event stream the run log expects: each step reports queued, then running,
// then exactly one terminal event, never a repeat and never a regression.
//
// A retried step has one key but several Argo nodes, one per attempt. The
// reporter remembers which node it last reported for each key and which nodes
// it has already reported terminal, so a later attempt is reported as a new
// run of the step even when it finished between two polls, and an earlier
// attempt is never reported twice.
type progressReporter struct {
	mu            sync.Mutex
	reported      map[string]runprogress.Status
	node          map[string]string
	done          map[string]bool
	replayed      map[string]bool
	ordinals      map[string]int
	ordinalOwners map[int]string
	failed        error
}

func newProgressReporter(destination io.Writer) *progressReporter {
	_ = destination
	return &progressReporter{
		reported:      map[string]runprogress.Status{},
		node:          map[string]string{},
		done:          map[string]bool{},
		replayed:      map[string]bool{},
		ordinals:      map[string]int{},
		ordinalOwners: map[int]string{},
	}
}

// assignOrdinal requires reporter.mu. Journal assignments survive adoption;
// newly named keys take an unused slot instead of displacing a sibling.
func (reporter *progressReporter) assignOrdinal(step StepResult) StepResult {
	key := step.Stage + "/" + step.Step
	if ordinal, exists := reporter.ordinals[key]; exists {
		step.Ordinal = ordinal
	} else {
		// Retain journal assignments across adoption, including old keys that
		// no longer occur in the Workflow. A newly canonicalized retry key
		// must never take a surviving sibling's durable ordinal.
		if _, used := reporter.ordinalOwners[step.Ordinal]; used {
			step.Ordinal = 0
			for {
				if _, used := reporter.ordinalOwners[step.Ordinal]; !used {
					break
				}
				step.Ordinal++
			}
		}
		reporter.ordinals[key] = step.Ordinal
		reporter.ordinalOwners[step.Ordinal] = key
	}
	return step
}

// order applies the same assignments to durable results and timing evidence
// as to progress markers, so completion cannot renumber an adopted run.
func (reporter *progressReporter) order(steps []StepResult) []StepResult {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	for i := range steps {
		steps[i] = reporter.assignOrdinal(steps[i])
	}
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].Ordinal < steps[j].Ordinal })
	return steps
}

// observe records a step's newly seen state and reports whether the caller
// should now replay its log (true only on the transition into a terminal
// state, so a log is written exactly once).
func (reporter *progressReporter) observe(destination io.Writer, step StepResult) bool {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	key := step.Stage + "/" + step.Step
	step = reporter.assignOrdinal(step)
	if step.NodeID != "" && reporter.done[step.NodeID] {
		// This attempt's terminal state is already in the run log.
		return false
	}
	previous, seen := reporter.reported[key]
	if seen && step.NodeID != "" && reporter.node[key] != "" && reporter.node[key] != step.NodeID {
		// Another Argo node under the same key: a different retry attempt.
		// Report it from the start, as a step seen for the first time.
		seen = false
	}
	if seen && !step.Superseded && terminal(previous) && terminal(step.Status) &&
		(previous != step.Status || (step.Attempt != "" && reporter.node[key] == "")) {
		// One node never changes its terminal state, so the step's current
		// row ending differently from what was reported is a later attempt.
		// For a retry with no restored node identity, replay the final attempt
		// even if its status matches: the stored status may be an older attempt.
		// This is what a reporter restored after a restart (#1172) sees:
		// it knows the key's last status but not which node produced it.
		seen = false
	}
	if seen && !advances(previous, step.Status) {
		// A retry attempt can restart a step that was previously terminal.
		// When a previously terminal step reports a non-terminal status,
		// that is a new attempt: reset tracking and let it flow through.
		if terminal(previous) && !terminal(step.Status) {
			// New retry attempt — fall through to emit.
		} else {
			if terminal(step.Status) && step.NodeID != "" {
				// Already reported, possibly before a restart restored
				// the reporter: never report this attempt again.
				reporter.done[step.NodeID] = true
			}
			return false
		}
	}
	if !seen && terminal(step.Status) && step.Status != runprogress.StepSkipped {
		// A step observed for the first time already finished (a fast step
		// between two polls). Emit its running event first so the log reads
		// as a sequence rather than a jump.
		reporter.emit(destination, StepResult{
			Stage: step.Stage, Step: step.Step, Ordinal: step.Ordinal,
			Status: runprogress.StepRunning, StartedAt: step.StartedAt,
		})
	}
	reporter.reported[key] = step.Status
	if step.NodeID != "" {
		reporter.node[key] = step.NodeID
		if terminal(step.Status) {
			reporter.done[step.NodeID] = true
		}
	}
	reporter.emit(destination, step)
	return terminal(step.Status)
}

func advances(previous, next runprogress.Status) bool {
	return rank(next) > rank(previous)
}

func rank(status runprogress.Status) int {
	switch status {
	case runprogress.StepQueued:
		return 0
	case runprogress.StepRunning:
		return 1
	default:
		return 2
	}
}

func (reporter *progressReporter) emit(destination io.Writer, step StepResult) {
	event := runprogress.Event{
		Version: runprogress.Version,
		Stage:   step.Stage, Step: step.Step, Ordinal: step.Ordinal,
		Status: step.Status, ExitCode: step.ExitCode,
		StartedAt: step.StartedAt, FinishedAt: step.FinishedAt,
	}
	// Unknown Argo exits use a display-only synthetic 1. Omit that exit from
	// durable progress so a second restart cannot promote it to real evidence.
	if !step.ExitCodeKnown {
		event.ExitCode = 0
	}
	// The progress contract requires timestamps consistent with the status.
	// Argo can report a terminal node before its timestamps settle, so fill
	// what is missing rather than dropping the transition.
	now := time.Now().UTC()
	if event.Status != runprogress.StepQueued && event.StartedAt == nil {
		event.StartedAt = &now
	}
	if terminal(event.Status) && event.FinishedAt == nil {
		event.FinishedAt = &now
	}
	if event.Status == runprogress.StepQueued {
		event.StartedAt, event.FinishedAt = nil, nil
	}
	if event.Status == runprogress.StepRunning {
		event.FinishedAt = nil
	}
	if event.FinishedAt != nil && event.StartedAt != nil && event.FinishedAt.Before(*event.StartedAt) {
		event.FinishedAt = event.StartedAt
	}
	marker, err := runprogress.MarshalMarker(event)
	if err != nil {
		reporter.failed = errors.Join(reporter.failed, err)
		return
	}
	if _, err := destination.Write(marker); err != nil {
		reporter.failed = errors.Join(reporter.failed, err)
	}
}

func (reporter *progressReporter) finish(destination io.Writer) {
	_ = destination
}

// failure returns the accumulated write/marshal errors from progress marker
// emission. A non-nil value means the run's durable progress record is
// degraded and the run must not report green.
func (reporter *progressReporter) failure() error {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	return reporter.failed
}

// specIdentity digests the exact object Oberth intends to submit, so an
// already-existing Workflow can be proven to be the same submission.
//
// Per-run source-volume fields (PVC claim name, subPaths for source, vault CA,
// and binary) are zeroed before hashing. These fields differ between
// the GET-first retry path (zero SourceVolume) and the create path (real
// SourceVolume), but do not change the pipeline's semantic content.
func specIdentity(workflow *wfv1.Workflow) (string, error) {
	clone := workflow.DeepCopy()
	delete(clone.Annotations, identityAnnotation)
	normalizeSourceVolume(clone)
	encoded, err := json.Marshal(clone)
	if err != nil {
		return "", fmt.Errorf("argojob: encode intended Workflow identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// normalizeSourceVolume removes the per-run source volume and all its
// dependents from the cloned workflow so that the GET-first retry path
// (zero SourceVolume, so Build adds no source volume at all) and the
// create path (real SourceVolume, so Build adds the PVC volume, source
// mount, vault-CA mount, binary mount, and VAULT_CACERT env var) produce
// identical digests. Removing rather than zeroing is necessary because
// the two paths differ structurally — one has the objects, the other
// does not — and zeroing fields cannot eliminate that difference.
func normalizeSourceVolume(workflow *wfv1.Workflow) {
	// Remove the source volume from Spec.Volumes.
	filtered := workflow.Spec.Volumes[:0:0]
	for _, v := range workflow.Spec.Volumes {
		if v.Name != SourceVolumeName {
			filtered = append(filtered, v)
		}
	}
	workflow.Spec.Volumes = filtered
	// Remove all mounts referencing the source volume and the
	// VAULT_CACERT env var (set only when vaultCADelivered is true,
	// which depends on the source volume's VaultCASubPath).
	removeMounts := func(mounts []corev1.VolumeMount) []corev1.VolumeMount {
		kept := mounts[:0:0]
		for _, m := range mounts {
			if m.Name != SourceVolumeName {
				kept = append(kept, m)
			}
		}
		return kept
	}
	removeVaultCACertEnv := func(env []corev1.EnvVar) []corev1.EnvVar {
		kept := env[:0:0]
		for _, e := range env {
			if e.Name != "VAULT_CACERT" {
				kept = append(kept, e)
			}
		}
		return kept
	}
	normalizeContainer := func(c *corev1.Container) {
		c.VolumeMounts = removeMounts(c.VolumeMounts)
		c.Env = removeVaultCACertEnv(c.Env)
	}
	for i := range workflow.Spec.Templates {
		tmpl := &workflow.Spec.Templates[i]
		if tmpl.Container != nil {
			normalizeContainer(tmpl.Container)
		}
		if tmpl.Script != nil {
			normalizeContainer(&tmpl.Script.Container)
		}
		if tmpl.ContainerSet != nil {
			for k := range tmpl.ContainerSet.Containers {
				normalizeContainer(&tmpl.ContainerSet.Containers[k].Container)
			}
		}
		for k := range tmpl.InitContainers {
			normalizeContainer(&tmpl.InitContainers[k].Container)
		}
		for k := range tmpl.Sidecars {
			normalizeContainer(&tmpl.Sidecars[k].Container)
		}
	}
}

// requestNeedsSecretStore mirrors Build's authored grant decision for seeding.
// Build reports malformed declarations and denied grants before submission.
func requestNeedsSecretStore(request Request) bool {
	workflow, err := argoworkflow.Decode(request.Source)
	if err != nil {
		return false
	}
	paths, err := argoworkflow.DeclaredSecretPaths(workflow)
	if err != nil || authorizeWithApprovalTable(paths, request) != nil {
		return false
	}
	steps, err := GrantedSecretSteps(workflow, paths, request.StepGrants)
	return err == nil && len(steps) > 0
}

var errEmptyExitCode = errors.New("argojob: node reported no usable exit code")
