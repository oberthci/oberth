package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/oberthci/oberth/internal/model"
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

const (
	waitIdleInterval = 15 * time.Second
	// mainContainerName is the container Argo runs the template's command in.
	mainContainerName = "main"
)

// Process initialization is the conservative boundary for restart attribution.
// A step first started later cannot have been cut by this server restart.
var serverStartedAt = time.Now().UTC()

// ErrNotTerminal reports that a Workflow has not reached a terminal phase.
var ErrNotTerminal = errors.New("argojob: Workflow has not reached a terminal state")

// Controller submits Workflows and supervises them to a terminal result.
type Controller struct {
	stepResults        StepResultWriter
	capabilities       func(context.Context, int64) (*argoworkflow.Capability, error)
	restartedAt        time.Time
	timings            TimingRecorder
	nodeBoots          NodeBootRecorder
	hub                ObservationHub
	workflows          WorkflowClient
	kube               kubernetes.Interface
	containerLogs      runlog.ContainerLogStore
	containerLogSource PodLogStream
	containerLogPod    func(context.Context, string, string) (*corev1.Pod, error)
	seeder             *SourceSeeder
	config             Config
	idleInterval       time.Duration
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

// Create submits one run directly. An existing fixed name is adopted only
// when its immutable run metadata identifies the same owner.
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
		if err := controller.sameCreateOwner(existing, request); err != nil {
			return "", err
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
		return "", &workflowCreateFailure{name: request.Name, stage: "lookup", cause: getErr, infra: transientWorkflowCreate(getErr)}
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

	created, rejected, createErr := controller.createWorkflow(ctx, request, intended)
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

// Wait supervises native Workflow watch observations, retaining node history
// and plain container output as the run proceeds.
func (controller *Controller) Wait(ctx context.Context, name, runID string, destination io.Writer) (Completion, error) {
	return controller.wait(ctx, name, runID, Adoption{})
}

// WaitResumed observes an adopted Workflow with its verified binding.
func (controller *Controller) WaitResumed(ctx context.Context, name, runID string, destination io.Writer, binding Adoption) (Completion, error) {
	if binding.UID == "" {
		binding.RestartedAt = time.Time{}
	}
	return controller.wait(ctx, name, runID, binding)
}

func (controller *Controller) wait(ctx context.Context, name, runID string, binding Adoption) (result Completion, retErr error) {
	observeLogs, finishLogs := controller.containerLogCapture(ctx, runID)
	defer finishLogs()
	if controller.hub == nil {
		return Completion{}, errors.New("argojob: shared Workflow watch is required")
	}
	wake, cancelWatch := controller.hub.Subscribe(name)
	defer cancelWatch()
	idle := controller.idleInterval
	if idle <= 0 {
		idle = waitIdleInterval
	}
	ticker := time.NewTicker(idle)
	defer ticker.Stop()
	delivered := map[types.UID]bool{}
	var admissionWatch executorIdentityProgress
	timed := map[string]bool{}
	boundTiming := false
	eventDriven := true
	confirmPending := false
	observationPaused := false
	nodeBoots := map[string]*corev1.Node{}
	recordedNodes := map[string]model.StepResult{}
	recordPending := false
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
					return Completion{}, fmt.Errorf("argojob: Workflow %s disappeared before completing; authored cleanup may have removed its result before observation", name)
				}
				if err != nil {
					confirmPending = true
					break
				}
				workflow = current
			}
			podSnapshot := controller.hub.PodsOf(name)
			pods := make([]corev1.Pod, 0, len(podSnapshot))
			for _, pod := range podSnapshot {
				pods = append(pods, *pod)
			}
			if err := controller.verifyOwnership(workflow, runID); err != nil {
				return Completion{}, err
			}
			if binding.UID != "" && !binding.matches(workflow) {
				return Completion{}, errors.New("argojob: adopted Workflow identity changed")
			}
			expanded, err := observedNodeWorkflow(ctx, workflow)
			if err != nil {
				return Completion{}, err
			}
			workflow = expanded
			// History failures remain retryable on events and idle ticks. The scheduler
			// persists final rows before FinishRun; observation never changes Argo's verdict.
			if eventDriven || recordPending {
				recordPending = controller.RecordNodeSteps(ctx, workflow, podSnapshot, recordedNodes) != nil
			}
			// Observe already-created Pods before identity delivery can block on a
			// later Pod: early step output must not wait behind that setup RPC.
			if eventDriven {
				observeLogs(workflow)
			}
			if eventDriven && !workflow.Status.Phase.Completed() {
				controller.observeRunningNodeBoots(ctx, workflow, nodeBoots)
			}
			if err := controller.observeWorkflowIdentities(ctx, workflow, pods, &admissionWatch, delivered); err != nil {
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
				settled, completeRead, settleErr := controller.confirmWatchedCompletion(ctx, workflow, runID, binding)
				if settleErr != nil {
					return Completion{}, settleErr
				}
				if settled == nil {
					confirmPending = true
					break
				}
				_ = controller.RecordNodeSteps(ctx, settled, podSnapshot, recordedNodes)
				controller.retainWorkflowTiming(ctx, settled)
				completion := completionFor(settled)
				completion.ObservationComplete = completion.ObservationComplete && completeRead
				preserveObservedExitCodes(&completion, workflow)
				controller.collectRestartStepDiagnostics(ctx, settled, &completion, binding.RestartedAt)
				observeLogs(settled)
				completion.classifyRestartCut()
				if err := controller.retireExecutorIdentity(ctx, settled); err != nil {
					return completion, fmt.Errorf("retire executor identity: %w", err)
				}

				return completion, nil
			}
			if eventDriven {
				controller.recordWatchedTimings(ctx, workflow, timed, &boundTiming)
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

// confirmWatchedCompletion uses a live read for the verdict and preserves the
// watched node set when the final status write has not caught up yet.
func (controller *Controller) confirmWatchedCompletion(ctx context.Context, observed *wfv1.Workflow, runID string, binding Adoption) (*wfv1.Workflow, bool, error) {
	var err error
	observed, err = observedNodeWorkflow(ctx, observed)
	if err != nil {
		return nil, false, err
	}
	settleCtx, cancelSettle := context.WithTimeout(ctx, 10*time.Second)
	settled, settleErr := controller.workflows.Get(settleCtx, observed.Name, metav1.GetOptions{})
	cancelSettle()
	if apierrors.IsNotFound(settleErr) {
		return observed.DeepCopy(), false, nil
	}
	if settleErr != nil {
		return nil, false, nil
	}
	if err := controller.verifyOwnership(settled, runID); err != nil {
		return nil, false, err
	}
	if observed.UID != "" && settled.UID != observed.UID {
		return nil, false, errors.New("argojob: Workflow identity changed at completion")
	}
	if !settled.Status.Phase.Completed() {
		return nil, false, nil
	}
	settled, err = observedNodeWorkflow(ctx, settled)
	if err != nil {
		return nil, false, err
	}
	completeRead := settled.Status.OffloadNodeStatusVersion == "" && len(settled.Status.Nodes) >= len(observed.Status.Nodes)
	if !completeRead && settled.Status.OffloadNodeStatusVersion == "" {
		copy := settled.DeepCopy()
		copy.Status.Nodes = observed.DeepCopy().Status.Nodes
		settled = copy
	}
	if binding.UID != "" && !binding.matches(settled) {
		return nil, false, errors.New("argojob: adopted Workflow identity changed at completion")
	}
	return settled, completeRead, nil
}

func (controller *Controller) recordWatchedTimings(ctx context.Context, workflow *wfv1.Workflow, timed map[string]bool, boundTiming *bool) {
	if controller.timings == nil {
		return
	}
	if !*boundTiming {
		controller.retainWorkflowTiming(ctx, workflow)
		*boundTiming = true
	}
	observation := Completion{}
	for _, step := range StepResults(workflow) {
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

// settle re-reads the Workflow once after it reports completion, preferring
// the later observation when it carries more terminal node results.
func (controller *Controller) settle(ctx context.Context, name, runID string, observed *wfv1.Workflow) (*wfv1.Workflow, bool) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	final, err := controller.workflows.Get(settleCtx, name, metav1.GetOptions{})
	if err != nil || controller.verifyOwnership(final, runID) != nil {
		return observed, false
	}
	final, err = observedNodeWorkflow(settleCtx, final)
	if err != nil {
		return observed, false
	}
	if final.Status.OffloadNodeStatusVersion != "" {
		return final, false
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
	workflow, err = observedNodeWorkflow(ctx, workflow)
	if err != nil {
		return nil, err
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
	return controller.shutdown(ctx, name, runID, wfv1.ShutdownStrategyStop)
}

// Terminate is Argo's immediate shutdown: exit handlers do not run.
func (controller *Controller) Terminate(ctx context.Context, name, runID string) error {
	return controller.shutdown(ctx, name, runID, wfv1.ShutdownStrategyTerminate)
}

func (controller *Controller) shutdown(ctx context.Context, name, runID string, strategy wfv1.ShutdownStrategy) error {
	var originalUID types.UID
	var lastConflict error
	for attempt := 0; attempt < 3; attempt++ {
		workflow, err := controller.workflows.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("argojob: read Workflow %s before shutdown: %w", name, err)
		}
		if err := controller.verifyOwnership(workflow, runID); err != nil {
			return err
		}
		if attempt == 0 {
			originalUID = workflow.UID
		} else if workflow.UID != originalUID {
			return fmt.Errorf("argojob: Workflow %s was replaced before shutdown", name)
		}
		_, err = requestWorkflowShutdown(ctx, controller.workflows, workflow, strategy)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if apierrors.IsConflict(err) {
			lastConflict = err
			continue
		}
		if err != nil {
			return fmt.Errorf("argojob: %s Workflow %s: %w", strategy, name, err)
		}
		return nil
	}
	return fmt.Errorf("argojob: Workflow %s kept changing during shutdown: %w", name, lastConflict)
}

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
