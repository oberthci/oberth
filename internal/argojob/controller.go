package argojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

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
)

// ErrNotTerminal reports that a Workflow has not reached a terminal phase.
var ErrNotTerminal = errors.New("argojob: Workflow has not reached a terminal state")

// errRunLogBudget is returned by the run budget writer when the aggregate
// per-run byte ceiling is exceeded.
var errRunLogBudget = errors.New("argojob: aggregate run log budget exceeded")

// Controller submits Workflows and supervises them to a terminal result.
type Controller struct {
	workflows WorkflowClient
	kube      kubernetes.Interface
	logs      PodLogReader
	seeder    *SourceSeeder
	config    Config
	interval  time.Duration
}

// NewController builds the engine. client may be nil when logs are supplied
// directly (tests); in production it is the same in-cluster Kubernetes client
// the Job engine uses, because step logs are ordinary pod logs.
func NewController(workflows WorkflowClient, client kubernetes.Interface, config Config) (*Controller, error) {
	if workflows == nil {
		return nil, errors.New("argojob: an Argo Workflow client is required")
	}
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return nil, err
	}
	controller := &Controller{workflows: workflows, kube: client, config: config, interval: pollInterval}
	if client != nil {
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
	return controller
}

// Namespace reports where this engine creates Workflows.
func (controller *Controller) Namespace() string { return controller.config.Namespace }

// Create submits one run's Workflow. Submission is idempotent against an
// identical object: a retry after an ambiguous create adopts the existing
// Workflow when it belongs to the same run and carries the same spec digest,
// and refuses otherwise rather than executing something that was not built
// from this run's reviewed source.
func (controller *Controller) Create(ctx context.Context, request Request) (string, error) {
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
		request, err = controller.refreshNonroot(ctx, request)
		if err != nil {
			return "", err
		}
		if request.nonrootProof != nil {
			request.SourceVolume = controller.nonrootExpectedSource(request)
		}
		// Build the intended spec so we can compare identity digests.
		// Use a zero SourceVolume; identity comparison excludes the
		// source claim name from the digest.
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
	volume, err := controller.seeder.Seed(ctx, request.Name, request.SourceDir, credentialed)
	if err != nil {
		return "", err
	}
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
	created, createErr := controller.workflows.Create(ctx, intended, metav1.CreateOptions{})
	if createErr == nil {
		controller.adoptClaimBestEffort(ctx, volume.ClaimName, created.Name, string(created.UID))
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
		return existing.Name, nil
	}
	// Non-definitive Create error: GET to check state.
	existing, getErr = controller.workflows.Get(ctx, intended.Name, metav1.GetOptions{})
	if getErr != nil {
		if apierrors.IsNotFound(getErr) {
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
	return existing.Name, nil
}

// adoptClaimBestEffort binds the claim's lifetime to the Workflow's so
// deleting the Workflow collects the storage. A failure leaves an unowned
// claim for SweepOrphanedSourceClaims rather than breaking a live run.
func (controller *Controller) adoptClaimBestEffort(ctx context.Context, claimName, workflowName, workflowUID string) {
	_ = controller.seeder.AdoptClaim(ctx, claimName, workflowName, workflowUID)
}

func sameSubmission(existing, intended *wfv1.Workflow) error {
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
	budget := &runBudgetWriter{destination: destination, remaining: controller.config.MaxRunLogBytes}
	reporter := newProgressReporter(budget)
	ticker := time.NewTicker(controller.interval)
	defer ticker.Stop()
	var budgetBreachErr error
	for {
		workflow, err := controller.workflows.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return Completion{}, fmt.Errorf("argojob: Workflow %s disappeared before completing", name)
			}
			return Completion{}, fmt.Errorf("argojob: read Workflow %s: %w", name, err)
		}
		if err := controller.verifyOwnership(workflow, runID); err != nil {
			return Completion{}, err
		}
		if publishErr := controller.publish(ctx, workflow, reporter, budget); publishErr != nil && budgetBreachErr == nil {
			budgetBreachErr = publishErr
		}
		if workflow.Status.Phase.Completed() {
			// One final read after the controller marks completion: the last
			// node transitions and the workflow phase are not written
			// atomically, so a status read that races the final update can
			// miss a step's terminal event.
			settled := controller.settle(ctx, name, runID, workflow)
			if publishErr := controller.publish(ctx, settled, reporter, budget); publishErr != nil && budgetBreachErr == nil {
				budgetBreachErr = publishErr
			}
			reporter.finish(budget)
			completion := completionFor(settled)
			if budgetBreachErr != nil {
				return completion, budgetBreachErr
			}
			if progressErr := reporter.failure(); progressErr != nil {
				return completion, fmt.Errorf("argojob: run progress markers degraded: %w", progressErr)
			}
			return completion, nil
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
	if workflow == nil {
		return nil
	}
	for _, step := range StepResults(workflow) {
		if !reporter.observe(destination, step) {
			continue
		}
		if terminal(step.Status) && step.PodName != "" {
			if err := controller.replayStepLog(ctx, workflow, step, destination); err != nil {
				return err
			}
		}
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
	if err := copyPrefixed(destination, step, io.LimitReader(stream, maxStepLogBytes)); err != nil {
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
	if _, _, ok := WorkflowMeta(workflow); !ok {
		return nil, ErrNotTerminal
	}
	if !workflow.Status.Phase.Completed() {
		return nil, ErrNotTerminal
	}
	completion := completionFor(workflow)
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
type progressReporter struct {
	mu       sync.Mutex
	reported map[string]runprogress.Status
	failed   error
}

func newProgressReporter(destination io.Writer) *progressReporter {
	_ = destination
	return &progressReporter{reported: map[string]runprogress.Status{}}
}

// observe records a step's newly seen state and reports whether the caller
// should now replay its log (true only on the transition into a terminal
// state, so a log is written exactly once).
func (reporter *progressReporter) observe(destination io.Writer, step StepResult) bool {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	key := step.Burn + "/" + step.Step
	previous, seen := reporter.reported[key]
	if seen && !advances(previous, step.Status) {
		// A retry attempt can restart a step that was previously terminal.
		// When a previously terminal step reports a non-terminal status,
		// that is a new attempt: reset tracking and let it flow through.
		if terminal(previous) && !terminal(step.Status) {
			// New retry attempt — fall through to emit.
		} else {
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
	destination io.Writer
	remaining   int64
}

func (writer *runBudgetWriter) Write(body []byte) (int, error) {
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
	prefix := []byte("[" + step.Burn + "/" + step.Step + "] ")
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
