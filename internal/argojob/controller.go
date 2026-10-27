package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"reflect"
	"sort"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
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

// PodLogReader fetches one pod's logs. Separated from the Kubernetes client so
// tests can supply canned output.
type PodLogReader func(ctx context.Context, namespace, pod, container string) (io.ReadCloser, error)

const (
	pollInterval        = 500 * time.Millisecond
	deletionGracePeriod = int64(10)
	// mainContainerName is the container Argo runs the template's command in.
	mainContainerName = "main"
	// maxStepLogBytes bounds one step's replayed log so a runaway step cannot
	// exhaust the run log budget on its own.
	maxStepLogBytes = 32 << 20
	// workflowReadGrace bounds how long Wait keeps polling through failed
	// Workflow reads (API server restart, connection reset) before it gives
	// up on observing the run.
	workflowReadGrace = 2 * time.Minute
)

// ErrNotTerminal reports that a Workflow has not reached a terminal phase.
var ErrNotTerminal = errors.New("argojob: Workflow has not reached a terminal state")

// errRunLogBudget is returned by the run budget writer when the aggregate
// per-run byte ceiling is exceeded.
var errRunLogBudget = errors.New("argojob: aggregate run log budget exceeded")

// Controller submits Workflows and supervises them to a terminal result.
type Controller struct {
	vmiQuotaMu        sync.Mutex
	profileVerifyMu   sync.Mutex
	profileMu         sync.Mutex
	profileStatus     model.ControllerProfileStatus
	profileBackoff    time.Duration
	profileLogAt      time.Time
	timings           TimingRecorder
	workflows         WorkflowClient
	kube              kubernetes.Interface
	logs              PodLogReader
	followLogs        PodLogFollower
	logIdentity       podLogIdentityReader
	seeder            *SourceSeeder
	config            Config
	vmi               dynamic.Interface
	vmiCreate         func(context.Context, string, *unstructured.Unstructured) (*unstructured.Unstructured, error)
	vmiCleanupFailure func(context.Context, string, error)
	interval          time.Duration
	readGrace         time.Duration
}

// WithVMIClient enables the run cleanup backstop for admitted VM resources.
func (controller *Controller) WithVMIClient(client dynamic.Interface) *Controller {
	controller.vmi = client
	return controller
}

// WithVMICleanupFailure records failures of the optional cleanup backstop.
func (controller *Controller) WithVMICleanupFailure(observe func(context.Context, string, error)) {
	controller.vmiCleanupFailure = observe
}

func (controller *Controller) reportVMICleanupFailure(ctx context.Context, runID string, err error) {
	log.Print("WARN: VMI cleanup backstop failed; see run audit for details")
	if controller.vmiCleanupFailure != nil {
		controller.vmiCleanupFailure(ctx, runID, err)
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
	controller := &Controller{workflows: workflows, kube: client, config: config, interval: pollInterval, readGrace: workflowReadGrace}
	if client != nil {
		controller.logIdentity = func(ctx context.Context, namespace, pod string) (string, error) {
			object, err := client.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
			if err != nil {
				return "", err
			}
			for _, status := range object.Status.ContainerStatuses {
				if status.Name == mainContainerName && status.ContainerID != "" {
					return fmt.Sprintf("%s/%s/%d", object.UID, status.ContainerID, status.RestartCount), nil
				}
			}
			return "", errors.New("main container identity is not available yet")
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
	attemptedCreate := false
	definitelyRejected := false
	if request.TrustedKVM != nil {
		defer func() {
			if resultErr != nil && (!attemptedCreate || definitelyRejected) {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				if err := controller.confirmTrustedKVMNoCreate(cleanupCtx, request.Name); err == nil {
					resultErr = errors.Join(ErrTrustedKVMCreateRefused, resultErr)
				}
			}
		}()
		if _, err := Build(controller.config, request); err != nil {
			return "", err
		}
	}

	var err error
	request, err = controller.refreshNonroot(ctx, request)
	if err != nil {
		return "", err
	}
	if controller.seeder == nil {
		return "", errors.New("argojob: no source seeder is configured; " +
			"a pipeline cannot mount the server's own claim across namespaces")
	}
	// Determine whether this pipeline is credentialed -- it will contact the
	// secret store -- so the seeder knows whether to deliver the trust anchor.
	// The document is decoded again inside Build; this lightweight pre-check
	// avoids restructuring the Seed-before-Build ordering.
	credentialed := request.Trigger == periapsis.TriggerRelease || pipelineDeclaresSecretPaths(request.Source)

	// GET the Workflow BEFORE any seeding. If the same submission already
	// exists, return/repair ownership without touching its claim. This
	// prevents a retry or name collision from rewriting a running Workflow's
	// source in place (finding 2, issue #29).
	existing, getErr := controller.workflows.Get(ctx, request.Name, metav1.GetOptions{})
	if getErr == nil {
		request.omitStepUsage = !workflowHasStepUsage(existing)
		request, err = controller.refreshNonroot(ctx, request)
		if err != nil {
			return "", err
		}
		request.SourceVolume = controller.expectedSourceVolume(request)
		// Reconstruct the original delivery footprint without reseeding. Legacy
		// workflows keep their unwrapped command; measured workflows retain the
		// same helper, including the exact nonroot executor mirror policy.
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
	request, err = controller.refreshNonroot(ctx, request)
	if err != nil {
		return "", err
	}
	controller.timingMilestone(ctx, request.RunID, "source-seed-started")
	volume, err := controller.seeder.seed(ctx, request.Name, request.SourceDir, credentialed, request.TrustedKVM == nil)
	if err != nil {
		return "", err
	}
	controller.timingMilestone(ctx, request.RunID, "source-ready")
	request.SourceVolume = volume
	request, err = controller.refreshNonroot(ctx, request)
	if err != nil {
		controller.seeder.DeleteClaim(context.WithoutCancel(ctx), volume.ClaimName)
		return "", err
	}

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
	attemptedCreate = true
	created, createErr := controller.workflows.Create(ctx, intended, metav1.CreateOptions{})
	if createErr == nil {
		controller.adoptClaimBestEffort(ctx, volume.ClaimName, created.Name, string(created.UID))
		controller.retainWorkflowTiming(ctx, created)
		return created.Name, nil
	}
	if apierrors.IsAlreadyExists(createErr) {
		// Race: the Workflow appeared between our GET and Create.
		existing, getErr = controller.workflows.Get(ctx, intended.Name, metav1.GetOptions{})
		if getErr != nil {
			return "", errors.Join(fmt.Errorf("argojob: create Workflow %s: %w", intended.Name, createErr), getErr)
		}
		if _, verifyErr := controller.refreshNonroot(ctx, request); verifyErr != nil {
			return "", verifyErr
		}
		if subErr := sameSubmission(existing, intended); subErr != nil {
			return "", subErr
		}
		controller.adoptClaimBestEffort(ctx, volume.ClaimName, existing.Name, string(existing.UID))
		controller.retainWorkflowTiming(ctx, existing)
		return existing.Name, nil
	}
	// Non-definitive Create error: GET to check state.
	existing, getErr = controller.workflows.Get(ctx, intended.Name, metav1.GetOptions{})
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
			definitelyRejected = apierrors.IsInvalid(createErr) || apierrors.IsForbidden(createErr) || apierrors.IsBadRequest(createErr)
			controller.seeder.DeleteClaim(context.WithoutCancel(ctx), volume.ClaimName)
		}
		return "", fmt.Errorf("argojob: create Workflow %s: %w", intended.Name, createErr)
	}
	if _, verifyErr := controller.refreshNonroot(ctx, request); verifyErr != nil {
		return "", verifyErr
	}
	if subErr := sameSubmission(existing, intended); subErr != nil {
		return "", subErr
	}
	controller.adoptClaimBestEffort(ctx, volume.ClaimName, existing.Name, string(existing.UID))
	controller.retainWorkflowTiming(ctx, existing)
	return existing.Name, nil
}

// adoptClaimBestEffort binds the claim's lifetime to the Workflow's so
// deleting the Workflow collects the storage. A failure leaves an unowned
// claim for SweepOrphanedSourceClaims rather than breaking a live run.
func (controller *Controller) adoptClaimBestEffort(ctx context.Context, claimName, workflowName, workflowUID string) {
	_ = controller.seeder.AdoptClaim(ctx, claimName, workflowName, workflowUID)
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

func sameSubmission(existing, intended *wfv1.Workflow) error {
	if binding := intended.Annotations[trustedKVMAnnotation]; binding != "" {
		if existing.Annotations[trustedKVMAnnotation] != binding || !reflect.DeepEqual(existing.Spec, intended.Spec) {
			return errors.New("argojob: existing Workflow differs from trusted KVM server spec")
		}
	}

	if binding := intended.Annotations[nonrootProfileAnnotation]; binding != "" {
		// Recovery cannot accept a forged identity annotation on old/root
		// semantics. Compare the actual stored spec with current server output,
		// including selected policies, source volumes and executor patch.
		if existing.Annotations[nonrootProfileAnnotation] != binding || !reflect.DeepEqual(existing.Spec, intended.Spec) {
			return errors.New("argojob: existing Workflow does not carry the verified nonroot policy and profile")
		}
	}
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
	return nil
}

// Wait supervises a submitted Workflow to its terminal phase, writing step
// progress markers and each step's pod log into destination as the run
// proceeds.
//
// Progress markers use the same wire protocol the Go path's runner emits, so
// the scheduler's existing step-progress writer, log index, and dashboard read
// an Argo run exactly as they read a Job run. The difference is only where the
// events come from: the Go runner emits them from inside the Job, while here
// the server derives them from the Workflow's node tree.
func (controller *Controller) Wait(ctx context.Context, name, runID string, destination io.Writer) (Completion, error) {
	return controller.wait(ctx, name, runID, destination, nil, 0, Adoption{})
}

// WaitResumed restores progress and the aggregate budget for an adopted Workflow.
func (controller *Controller) WaitResumed(ctx context.Context, name, runID string, destination io.Writer, events []runprogress.Event, retained int64, binding Adoption) (Completion, error) {
	return controller.wait(ctx, name, runID, destination, events, retained, binding)
}

func (controller *Controller) wait(ctx context.Context, name, runID string, destination io.Writer, events []runprogress.Event, retained int64, binding Adoption) (Completion, error) {
	if retained < 0 {
		return Completion{}, errors.New("argojob: invalid retained log size")
	}
	budget := &runBudgetWriter{destination: destination, remaining: controller.config.MaxRunLogBytes - retained}
	var live *stepLogFollowers
	if controller.followLogs != nil {
		live = newStepLogFollowers(ctx, controller.followLogs, budget, events)
		live.identity = controller.logIdentity
		live.goProxyURL = controller.config.GoProxyURL
		defer live.close()
	}
	reporter := newProgressReporter(budget)
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return Completion{}, err
		}
		reporter.reported[event.Burn+"/"+event.Step] = event.Status
		reporter.ordinals[event.Burn+"/"+event.Step] = event.Ordinal
		reporter.ordinalOwners[event.Ordinal] = event.Burn + "/" + event.Step
	}
	ticker := time.NewTicker(controller.interval)
	defer ticker.Stop()
	var budgetBreachErr error
	delivered := map[types.UID]bool{}
	var readFailingSince time.Time
	timed := map[string]bool{}
	boundTiming := false
	for {
		workflow, err := controller.workflows.Get(ctx, name, metav1.GetOptions{})
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
		if err := controller.deliverExecutorTokens(ctx, workflow, delivered); err != nil {
			return Completion{}, err
		}
		if workflow.Status.Phase.Completed() {
			// One final read after the controller marks completion: the last
			// node transitions and the workflow phase are not written
			// atomically, so a status read that races the final update can
			// miss a step's terminal event.
			settled := controller.settle(ctx, name, runID, workflow)
			if binding.UID != "" && !binding.matches(settled) {
				return Completion{}, errors.New("argojob: adopted Workflow identity changed at completion")
			}
			controller.retainWorkflowTiming(ctx, settled)
			completion := completionFor(settled)
			completion.Steps = reporter.order(completion.Steps)
			controller.collectStepDiagnostics(ctx, settled, &completion)
			controller.collectVMIFailure(ctx, settled, &completion, budget)
			if publishErr := controller.publishLogs(ctx, settled, reporter, budget, live); publishErr != nil && budgetBreachErr == nil {
				budgetBreachErr = publishErr
			}
			if live != nil {
				live.drain(settled)
				if err := live.failure(); err != nil && budgetBreachErr == nil {
					budgetBreachErr = err
				}
			}
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
			controller.cleanupVMIsForObservedWorkflow(ctx, settled, runID)
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
		if publishErr := controller.publishLogs(ctx, workflow, reporter, budget, live); publishErr != nil && budgetBreachErr == nil {
			budgetBreachErr = publishErr
		}

		select {
		case <-ctx.Done():
			return Completion{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// settle re-reads the Workflow once after it reports completion, preferring
// the later observation when it carries more terminal node results.
func (controller *Controller) settle(ctx context.Context, name, runID string, observed *wfv1.Workflow) *wfv1.Workflow {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	final, err := controller.workflows.Get(settleCtx, name, metav1.GetOptions{})
	if err != nil || controller.verifyOwnership(final, runID) != nil {
		return observed
	}
	if len(final.Status.Nodes) < len(observed.Status.Nodes) {
		return observed
	}
	return final
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

// publish emits the progress transitions and step logs this observation
// reveals that previous ones did not. It returns an error when the aggregate
// run log budget is breached during step log replay, naming the offending step.
func (controller *Controller) publish(ctx context.Context, workflow *wfv1.Workflow, reporter *progressReporter, destination io.Writer) error {
	return controller.publishLogs(ctx, workflow, reporter, destination, nil)
}

func (controller *Controller) publishLogs(ctx context.Context, workflow *wfv1.Workflow, reporter *progressReporter, destination io.Writer, live *stepLogFollowers) error {
	if workflow == nil {
		return nil
	}
	// Superseded retry attempts first: they ran before the attempt that
	// replaced them, and their failure and log are the evidence of why the
	// step was retried. They are reported once and never decide a verdict.
	// An attempt still non-terminal when superseded has nothing to report.
	observed := stepAttempts(workflow)
	steps := make([]StepResult, 0, len(observed))
	for _, step := range observed {
		if step.Superseded && terminal(step.Status) {
			steps = append(steps, step)
		}
	}
	steps = append(steps, finalStepResults(observed)...)
	for _, step := range steps {
		if !reporter.observe(destination, step) {
			continue
		}
		if live == nil && terminal(step.Status) && step.PodName != "" {
			if err := controller.replayStepLog(ctx, workflow, step, destination); err != nil {
				return err
			}
		}
	}
	if live != nil {
		live.observe(workflow)
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
func (controller *Controller) replayStepLog(ctx context.Context, workflow *wfv1.Workflow, step StepResult, destination io.Writer) error {
	if controller.logs == nil {
		return nil
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	stream, err := controller.logs(readCtx, workflow.Namespace, step.PodName, mainContainerName)
	if err != nil {
		writeLine(destination, step, fmt.Sprintf("oberth: step log unavailable: %v", err))
		return nil
	}
	defer func() { _ = stream.Close() }()
	if step.Attempt != "" {
		writeLine(destination, step, "oberth: Argo retry attempt "+step.Attempt)
	}
	if err := copyPrefixed(destination, step, io.LimitReader(stream, maxStepLogBytes), controller.config.GoProxyURL); err != nil {
		if errors.Is(err, errRunLogBudget) {
			// Best-effort banner -- the budget is exhausted so this write
			// will likely fail, but writeLine silently drops write errors.
			writeLine(destination, step, "oberth: run log truncated: aggregate budget exceeded")
			return fmt.Errorf("step %s/%s breached the aggregate run log budget", step.Burn, step.Step)
		}
		writeLine(destination, step, fmt.Sprintf("oberth: step log truncated: %v", err))
	}
	return nil
}

// TerminalState reports a Workflow's terminal result without requiring any
// in-process state, so a server that restarted mid-run can still record the
// real outcome instead of fabricating an interruption.
func (controller *Controller) TerminalState(ctx context.Context, name string) (*Completion, error) {
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
	if !workflow.Status.Phase.Completed() {
		return nil, ErrNotTerminal
	}
	completion := completionFor(workflow)
	controller.retainWorkflowTiming(ctx, workflow)
	controller.collectVMIFailure(ctx, workflow, &completion, nil)
	controller.collectStepDiagnostics(ctx, workflow, &completion)
	controller.cleanupVMIsForObservedWorkflow(ctx, workflow, runID)
	return &completion, nil
}

// Owns reports whether a Workflow with this name exists and belongs to Oberth.
// The dispatcher uses it to route a name back to its engine after a restart,
// when no in-process routing table survives.
func (controller *Controller) Owns(ctx context.Context, name string) (bool, error) {
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
		controller.cleanupVMIsBestEffort(ctx, name, runID)
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
	controller.cleanupVMIsForObservedWorkflow(ctx, workflow, runID)
	return nil
}

func (controller *Controller) cleanupVMIsForObservedWorkflow(ctx context.Context, workflow *wfv1.Workflow, runID string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := controller.cleanupVMIsForWorkflow(cleanupCtx, workflow, runID); err != nil {
		controller.reportVMICleanupFailure(cleanupCtx, runID, err)
	}
}

func (controller *Controller) cleanupVMIsBestEffort(ctx context.Context, name, runID string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := controller.CleanupVMIs(cleanupCtx, name, runID); err != nil {
		controller.reportVMICleanupFailure(cleanupCtx, runID, err)
	}
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
		ordinals:      map[string]int{},
		ordinalOwners: map[int]string{},
	}
}

// assignOrdinal requires reporter.mu. Journal assignments survive adoption;
// newly named keys take an unused slot instead of displacing a sibling.
func (reporter *progressReporter) assignOrdinal(step StepResult) StepResult {
	key := step.Burn + "/" + step.Step
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
	key := step.Burn + "/" + step.Step
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
	if !seen && terminal(step.Status) {
		// A step observed for the first time already finished (a fast step
		// between two polls). Emit its running event first so the log reads
		// as a sequence rather than a jump.
		reporter.emit(destination, StepResult{
			Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal,
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
		Burn:    step.Burn, Step: step.Step, Ordinal: step.Ordinal,
		Status: step.Status, ExitCode: step.ExitCode,
		StartedAt: step.StartedAt, FinishedAt: step.FinishedAt,
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
}

func (writer *runBudgetWriter) Write(body []byte) (int, error) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
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
func copyPrefixed(destination io.Writer, step StepResult, body io.Reader, proxyURL string) error {
	var diagnostic goProxyDiagnostic
	prefix := []byte("[" + step.Burn + "/" + step.Step + "] ")
	buffered := make([]byte, 0, 8192)
	chunk := make([]byte, 4096)
	var written int64
	flush := func(line []byte) error {
		assembled := make([]byte, 0, len(prefix)+len(line)+1)
		assembled = append(assembled, prefix...)
		assembled = append(assembled, line...)
		assembled = append(assembled, '\n')
		if hint := diagnostic.observe(string(line), proxyURL); hint != "" {
			assembled = append(assembled, prefix...)
			assembled = append(assembled, hint...)
			assembled = append(assembled, '\n')
		}
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
	_, _ = destination.Write([]byte("[" + step.Burn + "/" + step.Step + "] " + text + "\n"))
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
		c.Command = usageOriginalCommand(c.Command)
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

// pipelineDeclaresSecretPaths reports whether the pipeline document declares
// any secret-store paths. Decoding errors return false: Build will surface the
// real error when it decodes the same bytes.
func pipelineDeclaresSecretPaths(source []byte) bool {
	workflow, err := argoworkflow.Decode(source)
	if err != nil {
		return false
	}
	paths, err := argoworkflow.DeclaredSecretPaths(workflow)
	return err == nil && len(paths) > 0
}

var errEmptyExitCode = errors.New("argojob: node reported no usable exit code")
