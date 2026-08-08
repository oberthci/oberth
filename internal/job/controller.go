package job

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/redact"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runner"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	k8scontent "k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

const podDiscoveryInterval = 100 * time.Millisecond
const logCompletionGrace = 15 * time.Second
const retryInitial = 100 * time.Millisecond
const retryMaximum = 5 * time.Second
const emptyStreamRetryMaximum = time.Second
const jobCancellationTimeout = 30 * time.Second
const podProgressPatchTimeout = time.Second
const podProgressPatchInterval = 500 * time.Millisecond
const maximumPodProgressMarkerBytes = 2*k8scontent.LabelValueMaxLength + 3
const createReconcileTimeout = 5 * time.Second
const maximumPodLogReplayBytes = 128 << 20
const finalLogFetchAttempts = 5

const podProgressStepStartSuffix = " $ "

// LogStreamer opens one pod log stream. It must return promptly when ctx is
// canceled. The returned ReadCloser must allow Close concurrently with Read,
// and Close must promptly unblock an active Read so Wait can join its copier.
type LogStreamer func(context.Context, string, string) (io.ReadCloser, error)

type logCopyState struct {
	mu        sync.Mutex
	stream    io.ReadCloser
	finishing bool
	stopping  bool
}

var errPodLogReplayChanged = errors.New("pod log changed while reconnecting")
var errIncompleteFinalLog = errors.New("authoritative final Pod log did not complete")

type logDestinationError struct{ err error }

func (failure *logDestinationError) Error() string { return failure.err.Error() }
func (failure *logDestinationError) Unwrap() error { return failure.err }

type trackedLogDestination struct {
	writer io.Writer
	err    error
}

type replayLogWriter struct {
	history     *[]byte
	previous    int
	matched     int
	destination io.Writer
}

type podProgress struct {
	burn string
	step string
}

type podProgressReporter struct {
	updates chan podProgress
	done    chan struct{}
}

type logProgressWriter struct {
	reporter       *podProgressReporter
	marker         []byte
	startSuffix    int
	markerComplete bool
	skipLine       bool
	current        podProgress
}

func (destination *trackedLogDestination) Write(body []byte) (int, error) {
	written, err := destination.writer.Write(body)
	if err == nil && written != len(body) {
		err = io.ErrShortWrite
	}
	if err != nil && destination.err == nil {
		destination.err = err
	}
	return written, err
}

func (writer *replayLogWriter) Write(body []byte) (int, error) {
	written := 0
	if writer.matched < writer.previous {
		compare := min(len(body), writer.previous-writer.matched)
		if !bytes.Equal(body[:compare], (*writer.history)[writer.matched:writer.matched+compare]) {
			return written, fmt.Errorf("%w at byte %d", errPodLogReplayChanged, writer.matched)
		}
		writer.matched += compare
		written += compare
		body = body[compare:]
	}
	if len(body) == 0 {
		return written, nil
	}
	if len(*writer.history) > maximumPodLogReplayBytes-len(body) {
		return written, fmt.Errorf("pod log exceeds %d-byte replay safety limit", maximumPodLogReplayBytes)
	}
	appended, err := writer.destination.Write(body)
	if appended > 0 {
		*writer.history = append(*writer.history, body[:appended]...)
	}
	written += appended
	return written, err
}

func (writer *replayLogWriter) replayComplete() bool {
	return writer.matched == writer.previous
}

func newPodProgressReporter(ctx context.Context, controller *Controller, podName string) *podProgressReporter {
	reporter := &podProgressReporter{
		updates: make(chan podProgress, 1),
		done:    make(chan struct{}),
	}
	workerContext := context.WithoutCancel(ctx)
	go func() {
		defer close(reporter.done)
		var (
			latest       podProgress
			haveLatest   bool
			cooldown     *time.Timer
			cooldownChan <-chan time.Time
		)
		defer func() {
			if cooldown != nil {
				cooldown.Stop()
			}
		}()
		patchLatest := func() {
			patchContext, cancelPatch := context.WithTimeout(workerContext, podProgressPatchTimeout)
			_ = controller.patchPodProgress(patchContext, podName, latest)
			cancelPatch()
			haveLatest = false
			if cooldown == nil {
				cooldown = time.NewTimer(podProgressPatchInterval)
			} else {
				cooldown.Reset(podProgressPatchInterval)
			}
			cooldownChan = cooldown.C
		}
		for {
			select {
			case progress, open := <-reporter.updates:
				if !open {
					if haveLatest {
						patchLatest()
					}
					return
				}
				latest = progress
				haveLatest = true
				if cooldownChan == nil {
					patchLatest()
				}
			case <-cooldownChan:
				cooldownChan = nil
				if haveLatest {
					patchLatest()
				}
			}
		}
	}()
	return reporter
}

func (reporter *podProgressReporter) publish(progress podProgress) {
	select {
	case reporter.updates <- progress:
		return
	default:
	}
	select {
	case <-reporter.updates:
	default:
	}
	select {
	case reporter.updates <- progress:
	default:
	}
}

func (reporter *podProgressReporter) close() {
	close(reporter.updates)
}

func (writer *logProgressWriter) Write(body []byte) (int, error) {
	written := len(body)
	for len(body) > 0 {
		if writer.skipLine {
			newline := bytes.IndexByte(body, '\n')
			if newline < 0 {
				return written, nil
			}
			body = body[newline+1:]
			writer.marker = writer.marker[:0]
			writer.skipLine = false
			continue
		}
		character := body[0]
		body = body[1:]
		if character == '\n' {
			writer.marker = writer.marker[:0]
			writer.startSuffix = 0
			writer.markerComplete = false
			writer.skipLine = false
			continue
		}
		if writer.markerComplete {
			if character != podProgressStepStartSuffix[writer.startSuffix] {
				writer.marker = writer.marker[:0]
				writer.startSuffix = 0
				writer.markerComplete = false
				writer.skipLine = true
				continue
			}
			writer.startSuffix++
			if writer.startSuffix < len(podProgressStepStartSuffix) {
				continue
			}
			burn, step, found := runlog.ParseMarker(writer.marker)
			writer.marker = writer.marker[:0]
			writer.startSuffix = 0
			writer.markerComplete = false
			writer.skipLine = true
			progress := podProgress{burn: burn, step: step}
			if found && progress != writer.current {
				writer.current = progress
				writer.reporter.publish(progress)
			}
			continue
		}
		if len(writer.marker) == 0 && character != '[' {
			writer.skipLine = true
			continue
		}
		if len(writer.marker) >= maximumPodProgressMarkerBytes {
			writer.marker = writer.marker[:0]
			writer.skipLine = true
			continue
		}
		writer.marker = append(writer.marker, character)
		if character == ']' {
			writer.markerComplete = true
		}
	}
	return written, nil
}

func (state *logCopyState) attach(stream io.ReadCloser, final bool) bool {
	state.mu.Lock()
	if state.stopping || (state.finishing && !final) {
		state.mu.Unlock()
		_ = stream.Close()
		return false
	}
	state.stream = stream
	state.mu.Unlock()
	return true
}

func (state *logCopyState) finish() {
	state.mu.Lock()
	state.finishing = true
	stream := state.stream
	state.stream = nil
	state.mu.Unlock()
	if stream != nil {
		_ = stream.Close()
	}
}

func (state *logCopyState) shouldFinish() bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.finishing
}

func (state *logCopyState) release() error {
	state.mu.Lock()
	stream := state.stream
	state.stream = nil
	state.mu.Unlock()
	if stream != nil {
		return stream.Close()
	}
	return nil
}

func (state *logCopyState) stop() {
	state.mu.Lock()
	state.stopping = true
	stream := state.stream
	state.stream = nil
	state.mu.Unlock()
	if stream != nil {
		_ = stream.Close()
	}
}

type Controller struct {
	client             kubernetes.Interface
	config             Config
	streamLog          LogStreamer
	fetchLog           LogStreamer
	secretStore        SecretStoreFetcher
	execStream         ExecStreamer
	logCompletionGrace time.Duration
}

type Completion struct {
	JobName   string          `json:"job_name"`
	PodName   string          `json:"pod_name"`
	Succeeded bool            `json:"succeeded"`
	ExitCode  int32           `json:"exit_code"`
	Reason    string          `json:"reason"`
	Summary   json.RawMessage `json:"summary,omitempty"`
}

func NewController(client kubernetes.Interface, config Config, streamer LogStreamer, secretStore SecretStoreFetcher, exec ExecStreamer) (*Controller, error) {
	if client == nil {
		return nil, errors.New("kubernetes client is required")
	}
	if strings.TrimSpace(config.Namespace) == "" {
		return nil, errors.New("namespace is required")
	}
	applyDefaults(&config)
	if config.JobTimeout <= 0 {
		return nil, errors.New("job timeout must be greater than zero")
	}
	if err := validateReleaseSecretNames(config.ReleaseSecrets); err != nil {
		return nil, err
	}
	if err := validateSecretStorePathAllowlist(config.SecretStorePaths); err != nil {
		return nil, err
	}
	if len(config.SecretStorePaths) != 0 && (secretStore == nil || exec == nil) {
		return nil, errors.New("a secret store allowlist requires both a secret store fetcher and an exec streamer")
	}
	controller := &Controller{
		client: client, config: config, streamLog: streamer, fetchLog: streamer,
		secretStore: secretStore, execStream: exec,
		logCompletionGrace: logCompletionGrace,
	}
	if controller.streamLog == nil {
		controller.streamLog = func(ctx context.Context, namespace, pod string) (io.ReadCloser, error) {
			return client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: runnerContainerName, Follow: true}).Stream(ctx)
		}
		controller.fetchLog = func(ctx context.Context, namespace, pod string) (io.ReadCloser, error) {
			return client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: runnerContainerName}).Stream(ctx)
		}
	}
	return controller, nil
}

func (controller *Controller) Create(ctx context.Context, request Request) (string, error) {
	definition, err := Build(controller.config, request)
	if err != nil {
		return "", err
	}
	if err := controller.provisionRepositoryCaches(ctx, request); err != nil {
		return "", err
	}
	created, err := controller.ensureJob(ctx, definition)
	if err != nil {
		return "", err
	}
	if request.Release && len(request.ReleaseSecrets.Data) > 0 {
		if err := controller.ensureReleaseSnapshot(ctx, created, *request.ReleaseSecrets); err != nil {
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), jobCancellationTimeout)
			defer cancelCleanup()
			cleanupErr := controller.Cancel(cleanupCtx, created.Name, request.RunID)
			return "", errors.Join(fmt.Errorf("create immutable release Secret snapshot: %w", err), cleanupErr)
		}
	}
	return created.Name, nil
}

func (controller *Controller) Cancel(ctx context.Context, name, runID string) error {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(runID) == "" {
		return errors.New("job name and run ID are required")
	}
	jobs := controller.client.BatchV1().Jobs(controller.config.Namespace)
	current, err := controller.jobForCancellation(ctx, name)
	if err != nil {
		return err
	}
	var uid types.UID
	identity := ""
	if current != nil {
		if !jobBelongsToRun(current, runID) {
			return fmt.Errorf("cancel Job %s: object belongs to a different durable run", name)
		}
		uid = current.UID
		identity = current.Annotations[jobSpecIdentityAnnotation]
		policy := metav1.DeletePropagationForeground
		options := metav1.DeleteOptions{PropagationPolicy: &policy}
		if uid != "" {
			options.Preconditions = &metav1.Preconditions{UID: &uid}
		}
		deleteErr := jobs.Delete(ctx, name, options)
		if deleteErr != nil && !apierrors.IsNotFound(deleteErr) && !recoverableJobObservationError(deleteErr) {
			return fmt.Errorf("delete Job %s: %w", name, deleteErr)
		}
		if waitErr := controller.waitForCanceledJob(ctx, name, runID, uid, identity); waitErr != nil {
			if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
				return errors.Join(fmt.Errorf("delete Job %s: %w", name, deleteErr), waitErr)
			}
			return waitErr
		}
		return nil
	}
	return controller.waitForCanceledJob(ctx, name, runID, "", "")
}

func (controller *Controller) jobForCancellation(ctx context.Context, name string) (*batchv1.Job, error) {
	jobs := controller.client.BatchV1().Jobs(controller.config.Namespace)
	failures := 0
	for {
		current, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if err == nil {
			return current, nil
		}
		if !recoverableJobObservationError(err) {
			return nil, fmt.Errorf("get Job %s for cancellation: %w", name, err)
		}
		failures++
		if retryErr := waitForRetry(ctx, retryDelay(failures)); retryErr != nil {
			return nil, retryErr
		}
	}
}

func (controller *Controller) waitForCanceledJob(ctx context.Context, name, runID string, uid types.UID, identity string) error {
	jobs := controller.client.BatchV1().Jobs(controller.config.Namespace)
	selector := fields.OneTermEqualSelector("job-name", name).String()
	failures := 0
	for {
		current, jobErr := jobs.Get(ctx, name, metav1.GetOptions{})
		jobGone := apierrors.IsNotFound(jobErr)
		if jobErr != nil && !jobGone {
			if !recoverableJobObservationError(jobErr) {
				return fmt.Errorf("observe canceled Job %s: %w", name, jobErr)
			}
			failures++
			if retryErr := waitForRetry(ctx, retryDelay(failures)); retryErr != nil {
				return retryErr
			}
			continue
		}
		if !jobGone && !matchingCancellationTarget(current, runID, uid, identity) {
			return fmt.Errorf("observe canceled Job %s: name now identifies a different Job", name)
		}
		pods, podErr := controller.client.CoreV1().Pods(controller.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if podErr != nil {
			if !recoverableJobObservationError(podErr) {
				return fmt.Errorf("observe canceled Job %s pods: %w", name, podErr)
			}
			failures++
			if retryErr := waitForRetry(ctx, retryDelay(failures)); retryErr != nil {
				return retryErr
			}
			continue
		}
		activePod := false
		for index := range pods.Items {
			pod := &pods.Items[index]
			if !podOwnedByCancellationTarget(pod, name, runID, uid) {
				continue
			}
			if !runnerTerminatedState(pod) {
				activePod = true
				break
			}
		}
		if !activePod && (jobGone || terminalJob(current)) {
			return nil
		}
		failures = 0
		if retryErr := waitForRetry(ctx, podDiscoveryInterval); retryErr != nil {
			return retryErr
		}
	}
}

func matchingCancellationTarget(current *batchv1.Job, runID string, uid types.UID, identity string) bool {
	if current == nil {
		return false
	}
	if !jobBelongsToRun(current, runID) {
		return false
	}
	if uid != "" && current.UID != uid {
		return false
	}
	return identity == "" || current.Annotations[jobSpecIdentityAnnotation] == identity
}

func jobBelongsToRun(current *batchv1.Job, runID string) bool {
	if annotated, present := current.Annotations[jobRunIDAnnotation]; present {
		return annotated == runID
	}
	// A rolling upgrade can inherit a Job created immediately before the exact
	// run-ID annotation existed. Its deterministic persisted name is supplied
	// separately; require both legacy run labeling and a controller spec digest
	// before binding cancellation to the observed UID.
	return current.Labels["oberth.ci/run"] == labelValue(runID) && current.Annotations[jobSpecIdentityAnnotation] != ""
}

func podOwnedByCancellationTarget(pod *corev1.Pod, name, runID string, uid types.UID) bool {
	if uid == "" {
		if pod.Labels["job-name"] != name {
			return false
		}
		if annotated, present := pod.Annotations[jobRunIDAnnotation]; present {
			return annotated == runID
		}
		return pod.Labels["oberth.ci/run"] == labelValue(runID)
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" && owner.Name == name && owner.UID == uid {
			return true
		}
	}
	return false
}

func (controller *Controller) ensureJob(ctx context.Context, definition *batchv1.Job) (*batchv1.Job, error) {
	created, err := controller.client.BatchV1().Jobs(controller.config.Namespace).Create(ctx, definition, metav1.CreateOptions{})
	if err == nil {
		return created, nil
	}
	existing, getErr := getAfterAmbiguousCreate(ctx, func(reconcileCtx context.Context) (*batchv1.Job, error) {
		return controller.client.BatchV1().Jobs(controller.config.Namespace).Get(reconcileCtx, definition.Name, metav1.GetOptions{})
	})
	if getErr != nil {
		return nil, errors.Join(fmt.Errorf("create Job: %w", err), fmt.Errorf("reconcile Job after create: %w", getErr))
	}
	expectedIdentity := definition.Annotations[jobSpecIdentityAnnotation]
	if expectedIdentity == "" || existing.Annotations[jobSpecIdentityAnnotation] != expectedIdentity {
		return nil, fmt.Errorf("job %s already exists with a different spec identity", definition.Name)
	}
	return existing, nil
}

func (controller *Controller) ensureReleaseSnapshot(ctx context.Context, owner *batchv1.Job, snapshot SecretSnapshot) error {
	if owner == nil || owner.UID == "" {
		return errors.New("release Job UID is required before creating its Secret snapshot")
	}
	immutable, owned := true, true
	expected := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshot.Name, Namespace: controller.config.Namespace, Labels: maps.Clone(owner.Labels),
			Annotations: map[string]string{
				releaseSnapshotDigestAnnotation: snapshot.Digest,
				releaseSnapshotJobAnnotation:    owner.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: owner.Name, UID: owner.UID,
				Controller: &owned,
			}},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      cloneSecretData(snapshot.Data),
	}
	_, err := controller.client.CoreV1().Secrets(controller.config.Namespace).Create(ctx, expected, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	existing, getErr := getAfterAmbiguousCreate(ctx, func(reconcileCtx context.Context) (*corev1.Secret, error) {
		return controller.client.CoreV1().Secrets(controller.config.Namespace).Get(reconcileCtx, expected.Name, metav1.GetOptions{})
	})
	if getErr != nil {
		return errors.Join(fmt.Errorf("create release Secret snapshot: %w", err), fmt.Errorf("reconcile release Secret snapshot: %w", getErr))
	}
	if !matchingReleaseSnapshot(existing, expected) {
		return fmt.Errorf("release Secret snapshot %q exists with different immutable content or ownership", snapshot.Name)
	}
	return nil
}

func getAfterAmbiguousCreate[T any](ctx context.Context, get func(context.Context) (T, error)) (T, error) {
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createReconcileTimeout)
	defer cancel()
	var zero T
	var lastErr error
	for delay := retryInitial; ; delay = min(delay*2, retryMaximum) {
		value, err := get(reconcileCtx)
		if err == nil {
			return value, nil
		}
		lastErr = err
		timer := time.NewTimer(delay)
		select {
		case <-reconcileCtx.Done():
			timer.Stop()
			return zero, errors.Join(lastErr, reconcileCtx.Err())
		case <-timer.C:
		}
	}
}

func matchingReleaseSnapshot(existing, expected *corev1.Secret) bool {
	if existing == nil || expected == nil || existing.Name != expected.Name || existing.Namespace != expected.Namespace ||
		existing.Immutable == nil || !*existing.Immutable || existing.Type != corev1.SecretTypeOpaque ||
		!maps.Equal(existing.Labels, expected.Labels) || !maps.Equal(existing.Annotations, expected.Annotations) ||
		!maps.EqualFunc(existing.Data, expected.Data, bytes.Equal) || len(existing.OwnerReferences) != 1 {
		return false
	}
	owner, wanted := existing.OwnerReferences[0], expected.OwnerReferences[0]
	return owner.APIVersion == wanted.APIVersion && owner.Kind == wanted.Kind && owner.Name == wanted.Name && owner.UID == wanted.UID &&
		owner.Controller != nil && *owner.Controller
}

func cloneSecretData(values map[string][]byte) map[string][]byte {
	cloned := make(map[string][]byte, len(values))
	for key, value := range values {
		cloned[key] = slices.Clone(value)
	}
	return cloned
}

func (controller *Controller) Run(ctx context.Context, request Request, destination io.Writer, secretValues [][]byte, storePayload []byte) (Completion, error) {
	runContext, cancel := controller.waitContext(ctx)
	defer cancel()
	name, err := controller.Create(runContext, request)
	if err != nil {
		return Completion{}, err
	}
	return controller.Wait(runContext, name, request.RunID, destination, secretValues, storePayload)
}

func (controller *Controller) Wait(ctx context.Context, name, runID string, destination io.Writer, secretValues [][]byte, storePayload []byte) (Completion, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(runID) == "" {
		return Completion{}, errors.New("job name and run ID are required")
	}
	if destination == nil {
		destination = io.Discard
	}
	waitContext, cancelWait := controller.waitContext(ctx)
	defer cancelWait()

	// Store-sourced values reach the release Pod only through this exec
	// delivery; the runner blocks its burns until the delivery verifies, so
	// the worst failure mode is a fail-closed runner timeout, never a burn
	// with partial credentials.
	deliveryContext, cancelDelivery := context.WithCancel(waitContext)
	defer cancelDelivery()
	deliveryDone := make(chan error, 1)
	if len(storePayload) != 0 {
		go func() {
			deliveryDone <- controller.deliverSecretStoreSecrets(deliveryContext, runID, storePayload, secretValues)
		}()
	} else {
		deliveryDone <- nil
	}

	logContext, cancelLogs := context.WithCancel(waitContext)
	defer cancelLogs()
	type logResult struct {
		pod string
		err error
	}
	logDone := make(chan logResult, 1)
	logState := &logCopyState{}
	go func() {
		pod, err := controller.copyLogs(logContext, runID, destination, secretValues, logState)
		logDone <- logResult{pod: pod, err: err}
	}()

	type terminalResult struct {
		job *batchv1.Job
		err error
	}
	terminalDone := make(chan terminalResult, 1)
	go func() {
		terminal, err := controller.waitForTerminal(waitContext, name)
		terminalDone <- terminalResult{job: terminal, err: err}
	}()

	var terminal terminalResult
	var logs logResult
	logsFinished := false
	select {
	case terminal = <-terminalDone:
	case logs = <-logDone:
		logsFinished = true
		var destinationErr *logDestinationError
		fatalLogFailure := errors.As(logs.err, &destinationErr) || errors.Is(logs.err, errPodLogReplayChanged)
		if logs.err != nil && fatalLogFailure && (!isContextError(logs.err) || waitContext.Err() == nil) {
			cancelWait()
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), jobCancellationTimeout)
			cancelErr := controller.Cancel(cleanupCtx, name, runID)
			cancelCleanup()
			<-terminalDone
			return Completion{JobName: name, PodName: logs.pod}, errors.Join(fmt.Errorf("stream Job logs: %w", logs.err), cancelErr)
		}
		terminal = <-terminalDone
	case <-waitContext.Done():
		cancelLogs()
		logState.stop()
		terminal = <-terminalDone
		logs = <-logDone
		waitErr := waitContext.Err()
		if terminal.job == nil {
			waitErr = errors.Join(waitErr, controller.cancelAfterWaitFailure(name, runID))
		}
		return Completion{JobName: name, PodName: logs.pod}, waitErr
	}

	if terminal.err == nil {
		// A completed Job makes the non-following Pod log authoritative. Stop any
		// active follow request so the copier can fetch and verify that final log.
		logState.finish()
		grace := time.NewTimer(controller.logCompletionGrace)
		if !logsFinished {
			select {
			case logs = <-logDone:
				logsFinished = true
			case <-waitContext.Done():
				terminal.err = waitContext.Err()
			case <-grace.C:
				terminal.err = errIncompleteFinalLog
			}
		}
		if !grace.Stop() {
			select {
			case <-grace.C:
			default:
			}
		}
		if !logsFinished {
			cancelLogs()
			logState.stop()
			logs = <-logDone
		}
	} else if !logsFinished {
		cancelLogs()
		logState.stop()
		logs = <-logDone
	}
	collectDelivery := func() error {
		cancelDelivery()
		return <-deliveryDone
	}
	if terminal.err != nil {
		if terminal.job == nil {
			terminal.err = errors.Join(terminal.err, controller.cancelAfterWaitFailure(name, runID))
		}
		if deliveryErr := collectDelivery(); deliveryErr != nil && !isContextError(deliveryErr) {
			terminal.err = errors.Join(terminal.err, fmt.Errorf("secret store delivery: %w", deliveryErr))
		}
		return Completion{JobName: name, PodName: logs.pod}, terminal.err
	}
	completion, completionErr := controller.completion(waitContext, terminal.job, logs.pod)
	if deliveryErr := collectDelivery(); deliveryErr != nil && !completion.Succeeded && !isContextError(deliveryErr) {
		completionErr = errors.Join(completionErr, fmt.Errorf("secret store delivery: %w", deliveryErr))
	}
	if logs.err != nil {
		return completion, errors.Join(fmt.Errorf("stream Job logs: %w", logs.err), completionErr)
	}
	return completion, completionErr
}

func (controller *Controller) cancelAfterWaitFailure(name, runID string) error {
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), jobCancellationTimeout)
	defer cancelCleanup()
	return controller.Cancel(cleanupCtx, name, runID)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (controller *Controller) waitContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, controller.config.JobTimeout)
}

func (controller *Controller) SecretSnapshot(ctx context.Context, jobName string, requested []string) (SecretSnapshot, error) {
	if strings.TrimSpace(jobName) == "" || strings.ContainsAny(jobName, "\x00\r\n") {
		return SecretSnapshot{}, errors.New("release Job name is required for its Secret snapshot")
	}
	if err := validateReleaseSecretNames(requested); err != nil {
		return SecretSnapshot{}, fmt.Errorf("validate repository-declared release Secrets: %w", err)
	}
	allowed := make(map[string]struct{}, len(controller.config.ReleaseSecrets))
	for _, name := range controller.config.ReleaseSecrets {
		allowed[name] = struct{}{}
	}
	requested = slices.Clone(requested)
	slices.Sort(requested)
	for _, name := range requested {
		if _, ok := allowed[name]; !ok {
			return SecretSnapshot{}, fmt.Errorf("repository-declared release Secret %q is not in the administrator allowlist", name)
		}
	}
	snapshot := SecretSnapshot{
		Name:   releaseSnapshotName(jobName),
		Mounts: ReleaseSecretMounts{Secrets: make([]ReleaseSecret, 0, len(requested))},
		Data:   make(map[string][]byte),
	}
	for secretIndex, name := range requested {
		secret, err := controller.client.CoreV1().Secrets(controller.config.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return SecretSnapshot{}, fmt.Errorf("read release Secret %s for admission: %w", name, err)
		}
		keys := make([]string, 0, len(secret.Data))
		for key := range secret.Data {
			if err := validateReleaseSecretKey(name, key); err != nil {
				return SecretSnapshot{}, err
			}
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for keyIndex, key := range keys {
			snapshot.Data[secretSnapshotDataKey(secretIndex, keyIndex)] = slices.Clone(secret.Data[key])
		}
		snapshot.Mounts.Secrets = append(snapshot.Mounts.Secrets, ReleaseSecret{Name: name, Keys: keys})
	}
	digest, err := secretSnapshotDigest(snapshot)
	if err != nil {
		return SecretSnapshot{}, err
	}
	snapshot.Digest = digest
	return snapshot, nil
}

func (controller *Controller) copyLogs(ctx context.Context, runID string, destination io.Writer, values [][]byte, state *logCopyState) (string, error) {
	pod, err := controller.waitForPod(ctx, runID)
	if err != nil {
		return "", err
	}
	progressReporter := newPodProgressReporter(ctx, controller, pod.Name)
	defer progressReporter.close()
	progressWriter := &logProgressWriter{reporter: progressReporter}
	trackedDestination := &trackedLogDestination{writer: destination}
	redactor := redact.NewWriter(io.MultiWriter(trackedDestination, progressWriter), values)
	history := make([]byte, 0, 64<<10)
	failures := 0
	for {
		if state.shouldFinish() {
			return pod.Name, controller.copyFinalLogs(ctx, pod.Name, trackedDestination, redactor, &history, state)
		}
		stream, openErr := controller.streamLog(ctx, controller.config.Namespace, pod.Name)
		if openErr != nil {
			terminal, inspectErr := controller.runnerTerminated(ctx, pod.Name)
			if inspectErr != nil {
				if ctx.Err() != nil {
					return pod.Name, ctx.Err()
				}
				if recoverableJobObservationError(inspectErr) {
					failures++
					if retryErr := waitForRetry(ctx, retryDelay(failures)); retryErr != nil {
						return pod.Name, retryErr
					}
					continue
				}
				return pod.Name, errors.Join(openErr, inspectErr)
			}
			if terminal || state.shouldFinish() {
				return pod.Name, controller.copyFinalLogs(ctx, pod.Name, trackedDestination, redactor, &history, state)
			}
			failures++
			if retryErr := waitForRetry(ctx, retryDelay(failures)); retryErr != nil {
				return pod.Name, retryErr
			}
			continue
		}
		if !state.attach(stream, false) {
			if state.shouldFinish() {
				return pod.Name, controller.copyFinalLogs(ctx, pod.Name, trackedDestination, redactor, &history, state)
			}
			return pod.Name, contextError(ctx)
		}
		copied, replayComplete, copyErr := copyReplayStream(stream, redactor, &history)
		closeErr := state.release()
		streamErr := errors.Join(copyErr, closeErr)
		if state.shouldFinish() {
			return pod.Name, controller.copyFinalLogs(ctx, pod.Name, trackedDestination, redactor, &history, state)
		}
		if ctx.Err() != nil {
			return pod.Name, ctx.Err()
		}
		if trackedDestination.err != nil {
			return pod.Name, &logDestinationError{err: trackedDestination.err}
		}
		if errors.Is(streamErr, errPodLogReplayChanged) {
			return pod.Name, streamErr
		}
		terminal, inspectErr := controller.runnerTerminated(ctx, pod.Name)
		if inspectErr != nil {
			if ctx.Err() != nil {
				return pod.Name, ctx.Err()
			}
			if recoverableJobObservationError(inspectErr) {
				failures++
				if retryErr := waitForRetry(ctx, retryDelay(failures)); retryErr != nil {
					return pod.Name, retryErr
				}
				continue
			}
			return pod.Name, errors.Join(streamErr, inspectErr)
		}
		if terminal {
			return pod.Name, controller.copyFinalLogs(ctx, pod.Name, trackedDestination, redactor, &history, state)
		}
		failures++
		delay := retryDelay(failures)
		if streamErr == nil && copied == 0 && replayComplete && runnerWaitingState(pod) {
			delay = emptyStreamRetryDelay(failures)
		}
		if retryErr := waitForRetry(ctx, delay); retryErr != nil {
			return pod.Name, retryErr
		}
	}
}

func (controller *Controller) copyFinalLogs(
	ctx context.Context,
	podName string,
	destination *trackedLogDestination,
	redactor *redact.Writer,
	history *[]byte,
	state *logCopyState,
) error {
	var lastErr error
	for attempt := 1; attempt <= finalLogFetchAttempts; attempt++ {
		stream, openErr := controller.fetchLog(ctx, controller.config.Namespace, podName)
		if openErr != nil {
			lastErr = openErr
		} else if !state.attach(stream, true) {
			return contextError(ctx)
		} else {
			_, replayComplete, copyErr := copyReplayStream(stream, redactor, history)
			closeErr := state.release()
			lastErr = errors.Join(copyErr, closeErr)
			if destination.err != nil {
				return &logDestinationError{err: destination.err}
			}
			if errors.Is(lastErr, errPodLogReplayChanged) {
				return lastErr
			}
			if lastErr == nil && replayComplete {
				if closeErr := redactor.Close(); closeErr != nil {
					if destination.err != nil {
						return &logDestinationError{err: destination.err}
					}
					return closeErr
				}
				return nil
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("%w: final log ended before byte %d", errPodLogReplayChanged, len(*history))
			}
		}
		if attempt < finalLogFetchAttempts {
			if retryErr := waitForRetry(ctx, retryDelay(attempt)); retryErr != nil {
				return retryErr
			}
		}
	}
	return fmt.Errorf("fetch authoritative final Pod log after %d attempts: %w", finalLogFetchAttempts, lastErr)
}

func copyReplayStream(stream io.Reader, destination io.Writer, history *[]byte) (int, bool, error) {
	previous := len(*history)
	replay := &replayLogWriter{history: history, previous: previous, destination: destination}
	_, err := io.Copy(replay, stream)
	return len(*history) - previous, replay.replayComplete(), err
}

func contextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.Canceled
}

func (controller *Controller) patchPodProgress(ctx context.Context, podName string, progress podProgress) error {
	burnLabel, stepLabel := labelValue(progress.burn), labelValue(progress.step)
	if burnLabel == "" || stepLabel == "" {
		return errors.New("runner progress cannot form Kubernetes label values")
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"labels": map[string]string{
			"oberth.ci/burn": burnLabel,
			"oberth.ci/step": stepLabel,
		},
		"annotations": map[string]string{
			"oberth.ci/current-burn": progress.burn,
			"oberth.ci/current-step": progress.step,
		},
	}})
	if err != nil {
		return fmt.Errorf("encode Pod progress metadata: %w", err)
	}
	_, err = controller.client.CoreV1().Pods(controller.config.Namespace).Patch(ctx, podName, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (controller *Controller) runnerTerminated(ctx context.Context, podName string) (bool, error) {
	pod, err := controller.client.CoreV1().Pods(controller.config.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("inspect runner state: %w", err)
	}
	return runnerTerminatedState(pod), nil
}

func runnerTerminatedState(pod *corev1.Pod) bool {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == runnerContainerName && status.State.Terminated != nil {
			return true
		}
	}
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func runnerWaitingState(pod *corev1.Pod) bool {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != runnerContainerName {
			continue
		}
		if status.State.Waiting != nil {
			return true
		}
		if status.State.Running != nil || status.State.Terminated != nil {
			return false
		}
		break
	}
	// Kubernetes can publish the Pod object before its first container status.
	// Treat only that Pending/empty phase as the same pre-start race.
	return pod.Status.Phase == "" || pod.Status.Phase == corev1.PodPending
}

func (controller *Controller) waitForPod(ctx context.Context, runID string) (*corev1.Pod, error) {
	selector := fields.OneTermEqualSelector("oberth.ci/run", labelValue(runID)).String()
	failures := 0
	for {
		pods, err := controller.client.CoreV1().Pods(controller.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			if !recoverableJobObservationError(err) {
				return nil, fmt.Errorf("list Job pods: %w", err)
			}
			failures++
			if retryErr := waitForRetry(ctx, retryDelay(failures)); retryErr != nil {
				return nil, retryErr
			}
			continue
		}
		failures = 0
		if len(pods.Items) > 0 {
			return &pods.Items[0], nil
		}
		if retryErr := waitForRetry(ctx, podDiscoveryInterval); retryErr != nil {
			return nil, retryErr
		}
	}
}

func (controller *Controller) waitForTerminal(ctx context.Context, name string) (*batchv1.Job, error) {
	jobs := controller.client.BatchV1().Jobs(controller.config.Namespace)
	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	recoveryFailures := 0
	for {
		current, err := jobs.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if recoverableJobObservationError(err) {
				recoveryFailures++
				if retryErr := waitForRetry(ctx, retryDelay(recoveryFailures)); retryErr != nil {
					return nil, retryErr
				}
				continue
			}
			return nil, fmt.Errorf("get Job %s: %w", name, err)
		}
		if terminalJob(current) {
			return current, nil
		}
		stream, err := jobs.Watch(ctx, metav1.ListOptions{FieldSelector: selector, ResourceVersion: current.ResourceVersion})
		if err == nil {
			recoverWatch := false
			for !recoverWatch {
				select {
				case <-ctx.Done():
					stream.Stop()
					return nil, ctx.Err()
				case event, open := <-stream.ResultChan():
					if !open || event.Type == watch.Error || event.Type == watch.Deleted {
						recoverWatch = true
						continue
					}
					job, ok := event.Object.(*batchv1.Job)
					if !ok || job.Name != name {
						continue
					}
					recoveryFailures = 0
					if terminalJob(job) {
						stream.Stop()
						return job, nil
					}
				}
			}
			stream.Stop()
		}
		recoveryFailures++
		if retryErr := waitForRetry(ctx, retryDelay(recoveryFailures)); retryErr != nil {
			return nil, retryErr
		}
	}
}

func recoverableJobObservationError(err error) bool {
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err) || errors.Is(err, io.EOF) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func retryDelay(failures int) time.Duration {
	delay := retryInitial
	for failures > 1 && delay < retryMaximum {
		if delay > retryMaximum/2 {
			return retryMaximum
		}
		delay *= 2
		failures--
	}
	if delay > retryMaximum {
		return retryMaximum
	}
	return delay
}

func emptyStreamRetryDelay(failures int) time.Duration {
	return min(retryDelay(failures), emptyStreamRetryMaximum)
}

func terminalJob(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) {
			return true
		}
	}
	return false
}

func (controller *Controller) completion(ctx context.Context, terminal *batchv1.Job, preferredPod string) (Completion, error) {
	result := Completion{JobName: terminal.Name, PodName: preferredPod, ExitCode: -1}
	for _, condition := range terminal.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		if condition.Type == batchv1.JobComplete {
			result.Succeeded = true
		}
		if condition.Type == batchv1.JobFailed {
			result.Reason = condition.Reason
		}
	}
	selector := fields.OneTermEqualSelector("job-name", terminal.Name).String()
	if terminal.Spec.Selector != nil {
		selector = metav1.FormatLabelSelector(terminal.Spec.Selector)
	}
	pods, err := controller.client.CoreV1().Pods(controller.config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return result, fmt.Errorf("list terminal Job pods: %w", err)
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if result.PodName != "" && pod.Name != result.PodName {
			continue
		}
		result.PodName = pod.Name
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != runnerContainerName || status.State.Terminated == nil {
				continue
			}
			result.ExitCode = status.State.Terminated.ExitCode
			if result.Reason == "" {
				result.Reason = status.State.Terminated.Reason
			}
			message := strings.TrimSpace(status.State.Terminated.Message)
			if message != "" {
				steps, err := runner.DecodeStepResults([]byte(message))
				if err != nil {
					return result, fmt.Errorf("decode runner termination message: %w", err)
				}
				if len(steps) == 0 {
					return result, errors.New("runner termination message has no step results")
				}
				result.Summary = json.RawMessage(message)
			}
			if result.Succeeded && result.ExitCode == 0 && len(result.Summary) == 0 {
				return result, errors.New("successful Job has no runner termination summary")
			}
			return result, nil
		}
	}
	if result.Succeeded {
		return result, errors.New("successful Job has no terminated runner status")
	}
	return result, errors.New("failed Job has no terminated runner status")
}
