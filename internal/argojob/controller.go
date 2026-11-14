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
// and Stop patches. It never changes the authored execution graph.
type WorkflowClient interface {
	Create(ctx context.Context, workflow *wfv1.Workflow, options metav1.CreateOptions) (*wfv1.Workflow, error)
	Get(ctx context.Context, name string, options metav1.GetOptions) (*wfv1.Workflow, error)
	Patch(ctx context.Context, name string, kind types.PatchType, data []byte, options metav1.PatchOptions, subresources ...string) (*wfv1.Workflow, error)
}

// PodLogReader fetches one pod's logs. Separated from the Kubernetes client so
// tests can supply canned output.
type PodLogReader func(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error)

const (
	waitIdleInterval = 15 * time.Second
	// mainContainerName is the container Argo runs the template's command in.
	mainContainerName = "main"
	// maxStepLogBytes bounds one step's replayed log so a runaway step cannot
	// exhaust the run log budget on its own.
	maxStepLogBytes = 32 << 20
)

// Process initialization is the conservative boundary for restart attribution.
// A step first started later cannot have been cut by this server restart.
var serverStartedAt = time.Now().UTC()

// ErrNotTerminal reports that a Workflow has not reached a terminal phase.
var ErrNotTerminal = errors.New("argojob: Workflow has not reached a terminal state")

// errRunLogBudget is returned by the run budget writer when the aggregate
// per-run byte ceiling is exceeded.
var errRunLogBudget = errors.New("argojob: aggregate run log budget exceeded")

// Controller submits Workflows and supervises them to a terminal result.
type Controller struct {
	capabilities func(context.Context, int64) (*argoworkflow.Capability, error)
	restartedAt  time.Time
	timings      TimingRecorder
	nodeBoots    NodeBootRecorder
	hub          ObservationHub
	workflows    WorkflowClient
	kube         kubernetes.Interface
	logs         PodLogReader
	followLogs   PodLogFollower
	logIdentity  podLogIdentityReader
	logPod       func(context.Context, string, string) (*corev1.Pod, error)
	seeder       *SourceSeeder
	config       Config
	idleInterval time.Duration
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
	controller := &Controller{restartedAt: serverStartedAt, workflows: workflows, kube: client, config: config, idleInterval: waitIdleInterval}
	if client != nil {
		controller.logPod = func(ctx context.Context, namespace, pod string) (*corev1.Pod, error) {
			return client.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
		}
		controller.followLogs = func(ctx context.Context, namespace, pod string, options corev1.PodLogOptions) (io.ReadCloser, error) {
			return client.CoreV1().Pods(namespace).GetLogs(pod, &options).Stream(ctx)
		}
		controller.logs = func(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error) {
			return client.CoreV1().Pods(namespace).
				GetLogs(pod, &corev1.PodLogOptions{Container: container}).Stream(ctx)
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

// WithPodLogReader replaces the log source. Used by tests and by callers that
// stream logs through an already-authenticated path.
func (controller *Controller) WithPodLogReader(reader PodLogReader) *Controller {
	controller.logs = reader
	controller.followLogs = nil
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

// Wait supervises a submitted Workflow to its terminal phase, writing step
// progress markers and each step's pod log into destination as the run
// proceeds.
//
// The server derives progress events from the Workflow node tree and writes
// the retained marker protocol consumed by the scheduler, index and dashboard.
func (controller *Controller) Wait(ctx context.Context, name, runID string, destination io.Writer) (Completion, error) {
	return controller.wait(ctx, name, runID, destination, nil, 0, Adoption{})
}

// WaitResumed restores progress and the aggregate budget for an adopted Workflow.
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
	if controller.hub == nil {
		return Completion{}, errors.New("argojob: shared Workflow watch is required")
	}
	wake, cancelWatch := controller.hub.Subscribe(name)
	defer cancelWatch()
	budget := &runBudgetWriter{destination: destination, remaining: controller.config.MaxRunLogBytes - retained}
	var live *stepLogFollowers
	if controller.followLogs != nil {
		live = newStepLogFollowers(ctx, controller.followLogs, budget, events)
		live.steps = controller.observedStepAttempts
		live.identity = controller.logIdentity
		live.pod = controller.logPod
		defer live.close()
	}
	reporter := newProgressReporter(budget)
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return Completion{}, err
		}
		reporter.reported[event.Stage+"/"+event.Step] = event.Status
		reporter.ordinals[event.Stage+"/"+event.Step] = event.Ordinal
		reporter.ordinalOwners[event.Ordinal] = event.Stage + "/" + event.Step
	}
	idle := controller.idleInterval
	if idle <= 0 {
		idle = waitIdleInterval
	}
	ticker := time.NewTicker(idle)
	defer ticker.Stop()
	var budgetBreachErr error
	delivered := map[types.UID]bool{}
	var admissionWatch executorIdentityProgress
	timed := map[string]bool{}
	boundTiming := false
	eventDriven := true
	confirmPending := false
	observationPaused := false
	nodeBoots := map[string]*corev1.Node{}
	for {
		for {
			if ctx.Err() != nil {
				return Completion{}, ctx.Err()
			}
			workflow, found := controller.hub.Workflow(name)
			healthy := controller.hub.Healthy()
			if !healthy {
				admissionWatch.identityReadFailingSince = time.Time{}
				observationPaused = true
			} else if observationPaused {
				admissionWatch.resumeAfterOutage(time.Now())
				observationPaused = false
			}
			// A terminal Workflow event can still be confirmed live when only the
			// Pod informer is unavailable. Unhealthy idle ticks do no live reads.
			if !healthy && (!found || !workflow.Status.Phase.Completed() || !eventDriven) {
				break
			}
			confirm := eventDriven || confirmPending
			confirmPending = false
			if !found {
				// A missing cache entry never proves deletion. Confirm on the initial
				// observation/event, retrying only an owed confirmation on idle.
				if !confirm {
					break
				}
				readCtx, cancelRead := context.WithTimeout(ctx, executorIdentityAttemptTimeout)
				current, err := controller.workflows.Get(readCtx, name, metav1.GetOptions{})
				cancelRead()
				if apierrors.IsNotFound(err) {
					return Completion{}, fmt.Errorf("argojob: Workflow %s disappeared before completing", name)
				}
				if err != nil {
					confirmPending = true
					break
				}
				workflow = current
			}
			pods := make([]corev1.Pod, 0)
			for _, pod := range controller.hub.PodsOf(name) {
				pods = append(pods, *pod)
			}
			if err := controller.verifyOwnership(workflow, runID); err != nil {
				return Completion{}, err
			}
			if binding.UID != "" && !binding.matches(workflow) {
				return Completion{}, errors.New("argojob: adopted Workflow identity changed")
			}
			// Observe already-created Pods before identity delivery can block on a
			// later Pod: early step output must not wait behind that setup RPC.
			if live != nil && eventDriven {
				live.observe(workflow)
			}
			if eventDriven && !workflow.Status.Phase.Completed() {
				controller.observeRunningNodeBoots(ctx, workflow, nodeBoots)
			}
			if err := controller.checkExecutorIdentityProgress(ctx, workflow, pods, &admissionWatch, delivered); err != nil {
				return completionFor(workflow), err
			}
			// Bound reads and identity transport even when no waiter observation exists.
			deliveryCtx, stopDelivery := context.WithTimeout(ctx, identityObservationTimeout)
			identityReadUnavailable := false
			deliveryErr := controller.deliverExecutorTokensObserved(deliveryCtx, workflow, pods, delivered, func() { identityReadUnavailable = true })
			if deliveryCtx.Err() != nil && ctx.Err() == nil {
				deliveryErr = &executorIdentityProgressError{reason: "Kubernetes executor identity observation unavailable for 2m"}
			}
			stopDelivery()
			if deliveryErr == nil {
				deliveryErr = admissionWatch.observeIdentityReads(identityReadUnavailable, time.Now())
			}
			confirmPending = identityReadUnavailable
			if err := deliveryErr; err != nil {
				// Preserve observed execution evidence; a delivery error on a later
				// Pod must not make already executed steps look like a pre-step failure.
				if failure, ok := err.(interface{ InfrastructureFailure() bool }); ok && failure.InfrastructureFailure() && !controller.hub.Healthy() {
					break
				}
				return completionFor(workflow), err
			}
			if workflow.Status.Phase.Completed() {
				if !confirm {
					break
				}
				// One final read after the controller marks completion: the last
				// node transitions and the workflow phase are not written
				// atomically, so a status read that races the final update can
				// miss a step's terminal event.
				settleCtx, cancelSettle := context.WithTimeout(ctx, 10*time.Second)
				settled, settleErr := controller.workflows.Get(settleCtx, name, metav1.GetOptions{})
				cancelSettle()
				if apierrors.IsNotFound(settleErr) {
					return Completion{}, fmt.Errorf("argojob: Workflow %s disappeared before completing", name)
				}
				if settleErr != nil {
					confirmPending = true
					break
				}
				if err := controller.verifyOwnership(settled, runID); err != nil {
					return Completion{}, err
				}
				if workflow.UID != "" && settled.UID != workflow.UID {
					return Completion{}, errors.New("argojob: Workflow identity changed at completion")
				}
				if !settled.Status.Phase.Completed() {
					confirmPending = true
					break
				}
				return controller.finishWatchedWorkflow(ctx, workflow, settled, binding, events, reporter, budget, live, budgetBreachErr)
			}
			if controller.timings != nil && eventDriven {
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
					controller.collectWatchedStepDiagnostics(ctx, workflow, &observation, time.Time{})
					for _, step := range observation.Steps {
						timed[step.NodeID] = true
					}
				}
			}
			if eventDriven {
				if publishErr := controller.publishLogs(ctx, workflow, reporter, budget, live); publishErr != nil && budgetBreachErr == nil {
					budgetBreachErr = publishErr
				}
			}
			break
		}
		select {
		case <-ctx.Done():
			return Completion{}, ctx.Err()
		case <-wake:
			eventDriven = true
		case <-ticker.C:
			eventDriven = false
		}
	}
}

func (controller *Controller) finishWatchedWorkflow(
	ctx context.Context,
	workflow, settled *wfv1.Workflow,
	binding Adoption,
	events []runprogress.Event,
	reporter *progressReporter,
	budget *runBudgetWriter,
	live *stepLogFollowers,
	budgetBreachErr error,
) (Completion, error) {
	completeRead := len(settled.Status.Nodes) >= len(workflow.Status.Nodes)
	if !completeRead {
		copy := settled.DeepCopy()
		copy.Status.Nodes = workflow.DeepCopy().Status.Nodes
		settled = copy
	}
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
	if publishErr := controller.publishLogs(ctx, settled, reporter, budget, live); publishErr != nil && budgetBreachErr == nil {
		budgetBreachErr = publishErr
	}
	if live != nil {
		live.drain(settled)
		live.applyDiagnostics(&completion)
		if err := live.failure(); err != nil && budgetBreachErr == nil {
			budgetBreachErr = err
		}
	}
	completion.classifyRestartCut()
	reporter.finish(budget)
	if err := controller.retireExecutorIdentity(ctx, settled); err != nil {
		return completion, fmt.Errorf("retire executor identity: %w", err)
	}
	if budgetBreachErr != nil {
		return completion, budgetBreachErr
	}
	if progressErr := reporter.failure(); progressErr != nil {
		return completion, fmt.Errorf("argojob: run progress markers degraded: %w", progressErr)
	}
	return completion, nil
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

func (controller *Controller) publishLogs(ctx context.Context, workflow *wfv1.Workflow, reporter *progressReporter, destination io.Writer, live *stepLogFollowers) error {
	if workflow == nil {
		return nil
	}
	// Superseded retry attempts first: they ran before the attempt that
	// replaced them, and their failure and log are the evidence of why the
	// step was retried. They are reported once and never decide a verdict.
	// An attempt still non-terminal when superseded has nothing to report.
	observed := controller.observedStepAttempts(ctx, workflow)
	steps := make([]StepResult, 0, len(observed))
	for _, step := range observed {
		if step.Superseded && terminal(step.Status) {
			steps = append(steps, step)
		}
	}
	steps = append(steps, finalStepResults(observed)...)
	for _, step := range steps {
		if reporter.observe(destination, step) {
			// A terminal event makes this log eligible; only a read finishes
			// replay. Missing discovery must remain eligible on later polls.
			reporter.replayed[step.NodeID] = false
		}
		replayed, eligible := reporter.replayed[step.NodeID]
		if live != nil || !eligible || replayed || !terminal(step.Status) || workflow.Status.Nodes[step.NodeID].Type != wfv1.NodeTypePod {
			continue
		}
		if step.PodName == "" {
			if workflow.Status.Phase.Completed() {
				writeLine(destination, step, "oberth: step log unavailable: Pod identity could not be observed")
			}
			continue
		}
		read, err := controller.replayStepLog(ctx, workflow, step, destination)
		if err != nil {
			return err
		}
		reporter.replayed[step.NodeID] = read
	}
	if live != nil {
		live.observeSteps(workflow, observed)
	}
	return nil
}

// replayStepLog copies one finished step's pod log into the run log, prefixed
// so the existing runlog index attributes every line to its burn and step.
//
// Steps are copied whole on completion rather than streamed concurrently:
// Argo runs each step in its own pod, and interleaving several live streams
// into one append-only log would produce output no reader could attribute.
//
// It returns an error only when the aggregate run log budget is breached: the
// per-step truncation is handled in-band (banner + continue), but an aggregate
// breach must propagate to Wait so the run goes red with step attribution.
func (controller *Controller) replayStepLog(ctx context.Context, workflow *wfv1.Workflow, step StepResult, destination io.Writer) (bool, error) {
	if controller.logs == nil {
		return false, nil
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	stream, err := controller.logs(readCtx, workflow.Namespace, step.PodName, mainContainerName)
	if err != nil {
		writeLine(destination, step, fmt.Sprintf("oberth: step log unavailable: %v", err))
		return false, nil
	}
	defer func() { _ = stream.Close() }()
	if step.Attempt != "" {
		writeLine(destination, step, "oberth: Argo retry attempt "+step.Attempt)
	}
	if err := copyPrefixed(destination, step, io.LimitReader(stream, maxStepLogBytes)); err != nil {
		if errors.Is(err, errRunLogBudget) {
			// Best-effort banner -- the budget is exhausted so this write
			// will likely fail, but writeLine silently drops write errors.
			writeLine(destination, step, "oberth: run log truncated: aggregate budget exceeded")
			return true, fmt.Errorf("step %s/%s breached the aggregate run log budget", step.Stage, step.Step)
		}
		writeLine(destination, step, fmt.Sprintf("oberth: step log truncated: %v", err))
	}
	return true, nil
}

// TerminalState reports a Workflow's terminal result without requiring any
// in-process state, so a server that restarted mid-run can still record the
// real outcome instead of fabricating an interruption.
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

// Cancel requests Argo Stop for this run. Argo executes exit handlers and owns
// the Workflow's eventual TTL cleanup; a foreign object is never patched.
func (controller *Controller) Cancel(ctx context.Context, name, runID string) error {
	if runID == "" {
		return errors.New("argojob: run ID is required to Stop a Workflow")
	}
	workflow, err := controller.workflows.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("argojob: read Workflow %s before stopping: %w", name, err)
	}
	if err := controller.verifyOwnership(workflow, runID); err != nil {
		return err
	}
	_, err = requestWorkflowStop(ctx, controller.workflows, workflow)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if apierrors.IsConflict(err) {
		// Concurrent Stop requests are idempotent, but a replaced UID must
		// never inherit this run's request.
		current, readErr := controller.workflows.Get(ctx, name, metav1.GetOptions{})
		if readErr == nil && current.UID == workflow.UID && controller.verifyOwnership(current, runID) == nil &&
			(current.Spec.Shutdown == wfv1.ShutdownStrategyStop || current.Status.Phase.Completed()) {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("argojob: Stop Workflow %s: %w", name, err)
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

// runBudgetWriter wraps a destination writer with an aggregate byte ceiling.
// When the ceiling is exceeded, subsequent writes fail with errRunLogBudget.
// This is the Argo engine's counterpart of service.runLogWriter: both enforce
// the same 64 MiB default, but this one lives inside the controller so it can
// propagate the breach as a step-attributed error through Wait.
type runBudgetWriter struct {
	mu          sync.Mutex
	destination io.Writer
	remaining   int64
	failure     error
}

func (writer *runBudgetWriter) Write(body []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.failure != nil {
		return 0, writer.failure
	}
	if writer.remaining <= 0 {
		return 0, errRunLogBudget
	}
	allowed := len(body)
	limited := int64(allowed) > writer.remaining
	if limited {
		allowed = int(writer.remaining)
	}
	written, err := writer.destination.Write(body[:allowed])
	writer.remaining -= int64(written)
	if err != nil {
		return written, err
	}
	if written != allowed {
		return written, io.ErrShortWrite
	}
	if limited {
		return written, errRunLogBudget
	}
	return written, nil
}

// copyPrefixed writes body into destination with a [burn/step] prefix on every
// line, which is the marker internal/runlog indexes on. It returns an error on
// the first destination write failure or when the transformed output exceeds
// the per-step budget, and stops reading immediately in either case.
func copyPrefixed(destination io.Writer, step StepResult, body io.Reader) error {
	prefix := []byte("[" + step.Stage + "/" + step.Step + "] ")
	buffered := make([]byte, 0, 8192)
	chunk := make([]byte, 4096)
	var written int64
	flush := func(line []byte) error {
		assembled := make([]byte, 0, len(prefix)+len(line)+1)
		assembled = append(assembled, prefix...)
		assembled = append(assembled, line...)
		assembled = append(assembled, '\n')
		written += int64(len(assembled))
		if written > maxStepLogBytes {
			return fmt.Errorf("step log exceeds the %d byte budget", maxStepLogBytes)
		}
		_, err := destination.Write(assembled)
		return err
	}
	for {
		read, err := body.Read(chunk)
		if read > 0 {
			buffered = append(buffered, chunk[:read]...)
			if int64(len(buffered)) > maxStepLogBytes {
				buffered = buffered[:maxStepLogBytes]
			}
			for {
				index := indexByte(buffered, '\n')
				if index < 0 {
					break
				}
				if flushErr := flush(trimCarriageReturn(buffered[:index])); flushErr != nil {
					return flushErr
				}
				buffered = buffered[index+1:]
			}
		}
		if err != nil {
			break
		}
	}
	if len(buffered) != 0 {
		return flush(trimCarriageReturn(buffered))
	}
	return nil
}

func writeLine(destination io.Writer, step StepResult, text string) {
	_, _ = destination.Write([]byte("[" + step.Stage + "/" + step.Step + "] " + text + "\n"))
}

func indexByte(body []byte, target byte) int {
	for index, value := range body {
		if value == target {
			return index
		}
	}
	return -1
}

func trimCarriageReturn(line []byte) []byte {
	if len(line) != 0 && line[len(line)-1] == '\r' {
		return line[:len(line)-1]
	}
	return line
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

func (writer *runBudgetWriter) PodLogCursors() map[string]runlog.PodLogCursor {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if sink, ok := writer.destination.(runlog.PodLogSink); ok {
		return sink.PodLogCursors()
	}
	return nil
}
func (writer *runBudgetWriter) CommitPodLog(cursor runlog.PodLogCursor, body []byte) (result error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.failure != nil {
		return writer.failure
	}
	defer func() {
		if result != nil {
			writer.failure = result
		}
	}()
	if int64(len(body)) > writer.remaining {
		return errRunLogBudget
	}
	if sink, ok := writer.destination.(runlog.PodLogSink); ok {
		if err := sink.CommitPodLog(cursor, body); err != nil {
			return err
		}
		writer.remaining -= int64(len(body))
		return nil
	}
	n, err := writer.destination.Write(body)
	writer.remaining -= int64(n)
	if err == nil && n != len(body) {
		err = io.ErrShortWrite
	}
	return err
}

func (writer *runBudgetWriter) PodLogContinuityLost() bool {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if state, ok := writer.destination.(interface{ PodLogContinuityLost() bool }); ok {
		return state.PodLogContinuityLost()
	}
	return false
}
