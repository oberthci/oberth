package argojob

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
)

// PodLogFollower must honor cancellation and return Kubernetes timestamped
// records. SinceTime is inclusive; a timestamp alone is not a unique cursor.
type PodLogFollower func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error)

func (controller *Controller) WithPodLogFollower(reader PodLogFollower) *Controller {
	controller.followLogs = reader
	controller.logIdentity = nil
	controller.logPod = nil
	return controller
}

type podLogIdentityReader func(context.Context, string, string) (string, error)

const (
	maxLogFollowers      = 32
	maxLogRecordBytes    = 1 << 20
	followLogIdleTimeout = 45 * time.Second
	finalLogReadTimeout  = 60 * time.Second
	finalLogDrainBudget  = 120 * time.Second
)

// Only complete attributed lines reach the shared, serialized budget writer.
// No pod can interleave a partial line with another pod or a progress marker.
// A bounded read lease also prevents a stalled connection blocking final drain.
type stepLogFollowers struct {
	goProxyURL   string
	ctx          context.Context
	cancel       context.CancelFunc
	reader       PodLogFollower
	identity     podLogIdentityReader
	pod          func(context.Context, string, string) (*corev1.Pod, error)
	restored     map[string]runlog.PodLogCursor
	drainExpired atomic.Bool
	output       io.Writer
	workers      map[string]*stepLogFollower
	resumed      map[string]runprogress.Status
	resumedAt    time.Time
	mu           sync.Mutex
	err          error
}

type stepLogFollower struct {
	proxyDiagnostic     goProxyDiagnostic
	owner               *stepLogFollowers
	step                StepResult
	namespace           string
	terminal            atomic.Bool
	followMu            sync.Mutex
	followCancel        context.CancelFunc
	done                chan struct{}
	cursor              logCursor
	bytes               int64
	state               string
	stateMessage        string
	lastGap             string
	gap                 bool
	skipBoundary        bool
	identity            string
	workflow            *wfv1.Workflow
	node                wfv1.NodeStatus
	latestNode          atomic.Pointer[wfv1.NodeStatus]
	key                 string
	container           string
	diagnostic          *model.StepDiagnostic
	terminationReported bool
	complete            bool
	pending             []byte
}

func newStepLogFollowers(ctx context.Context, reader PodLogFollower, output io.Writer, events []runprogress.Event) *stepLogFollowers {
	ctx, cancel := context.WithCancel(ctx)
	resumed := map[string]runprogress.Status{}
	for _, event := range events {
		resumed[event.Burn+"/"+event.Step] = event.Status
	}
	var restored map[string]runlog.PodLogCursor
	if sink, ok := output.(runlog.PodLogSink); ok {
		restored = sink.PodLogCursors()
	}
	return &stepLogFollowers{restored: restored, ctx: ctx, cancel: cancel, reader: reader, output: output, workers: map[string]*stepLogFollower{}, resumed: resumed, resumedAt: time.Now().UTC()}
}

func (logs *stepLogFollowers) observe(workflow *wfv1.Workflow) {
	active := 0
	busySteps := map[string]bool{}
	for _, worker := range logs.workers {
		select {
		case <-worker.done:
		default:
			active++
			busySteps[worker.step.Burn+"/"+worker.step.Step] = true
		}
	}
	for _, step := range stepAttempts(workflow) {
		if step.PodName == "" || (step.Status != runprogress.StepQueued && step.Status != runprogress.StepRunning && !terminal(step.Status)) {
			continue
		}
		key := step.NodeID + "/" + step.PodName
		if worker := logs.workers[key]; worker != nil {
			if terminal(step.Status) {
				node := workflow.Status.Nodes[step.NodeID]
				worker.latestNode.Store(&node)
				worker.markTerminal()
			}
			continue
		}
		// Overflow stays eligible on the next observation and is always collected
		// during drain. Do not allocate one goroutine or buffer per pending leaf.
		if active >= maxLogFollowers || busySteps[step.Burn+"/"+step.Step] {
			continue
		}
		worker := &stepLogFollower{owner: logs, step: step, namespace: workflow.Namespace, done: make(chan struct{}), workflow: workflow, node: workflow.Status.Nodes[step.NodeID], key: string(workflow.UID) + "/" + key, container: mainContainerName}
		if saved, ok := logs.restored[worker.key]; ok && saved.WorkflowUID == string(workflow.UID) && saved.Node == step.NodeID && saved.Pod == step.PodName {
			worker.cursor = logCursor{at: saved.At, count: saved.Count, digest: saved.Digest}
			worker.bytes = saved.Bytes
			worker.identity = saved.Identity
			worker.container = saved.Container
			worker.gap = saved.Gap
			worker.skipBoundary = saved.SkipBoundary
			worker.complete = saved.Complete
			worker.diagnostic = saved.Termination
			worker.terminationReported = saved.TerminationReported
		}
		worker.terminal.Store(terminal(step.Status))
		logs.workers[key] = worker
		active++
		busySteps[step.Burn+"/"+step.Step] = true
		go worker.run()
	}
}

func (logs *stepLogFollowers) close() {
	logs.cancel()
	for _, worker := range logs.workers {
		<-worker.done
	}
}

func (logs *stepLogFollowers) drain(workflow *wfv1.Workflow) {
	timer := time.AfterFunc(finalLogDrainBudget, func() { logs.drainExpired.Store(true); logs.cancel() })
	defer timer.Stop()

	// Finished-between-observations steps and overflow are drained in bounded
	// batches. A pod log error degrades delivery, never the command verdict.
	for {
		logs.observe(workflow)
		for _, worker := range logs.workers {
			worker.markTerminal()
		}
		for _, worker := range logs.workers {
			<-worker.done
		}
		before := len(logs.workers)
		logs.observe(workflow)
		if len(logs.workers) == before {
			return
		}
	}
}

func (logs *stepLogFollowers) failure() error {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return logs.err
}

func (worker *stepLogFollower) status(state, message string) {
	if state == worker.state && message == worker.stateMessage {
		return
	}
	worker.state = state
	worker.stateMessage = message
	// Separate control namespace: these records are delivery observations, not
	// fabricated command output. Step readers continue indexing only [burn/step].
	body, _ := json.Marshal(struct {
		Burn    string    `json:"burn"`
		Step    string    `json:"step"`
		Node    string    `json:"node"`
		State   string    `json:"state"`
		At      time.Time `json:"at"`
		Message string    `json:"message,omitempty"`
		Gap     bool      `json:"gap,omitempty"`
	}{worker.step.Burn, worker.step.Step, worker.step.NodeID, state, time.Now().UTC(), message, worker.gap})
	worker.write(append(append([]byte(runprogress.LogDeliveryPrefix), body...), '\n'))
}

func (worker *stepLogFollower) write(body []byte) bool {
	if _, err := worker.owner.output.Write(body); err != nil {
		worker.owner.mu.Lock()
		worker.owner.err = errors.Join(worker.owner.err, fmt.Errorf("step %s/%s log write: %w", worker.step.Burn, worker.step.Step, err))
		worker.owner.mu.Unlock()
		return false
	}
	return true
}

func (worker *stepLogFollower) discontinuity(message string) {
	worker.gap = true
	if worker.lastGap == message {
		return
	}
	worker.lastGap = message
	worker.status("gap", message)
	// Retain the diagnostic with the step too, so CLI/MCP readers see gaps.
	worker.write([]byte("[" + worker.step.Burn + "/" + worker.step.Step + "] oberth: log continuity gap: " + message + "\n"))
}

func (worker *stepLogFollower) run() {
	defer close(worker.done)

	_, hasCursor := worker.owner.restored[worker.key]
	if worker.complete {
		return
	}
	if worker.step.Attempt != "" && !hasCursor {
		worker.write([]byte("[" + worker.step.Burn + "/" + worker.step.Step + "] oberth: Argo retry attempt " + worker.step.Attempt + "\n"))
	}

	if previous, restored := worker.owner.resumed[worker.step.Burn+"/"+worker.step.Step]; restored && !hasCursor && (worker.step.StartedAt == nil || worker.step.StartedAt.Before(worker.owner.resumedAt)) {
		if terminal(previous) && worker.step.Attempt == "" {
			worker.discontinuity("controller adopted a previously terminal step; its retained output is preserved without historical replay")
			worker.status("gapped", "historical log completeness cannot be established after adoption")
			return
		}
		// There is no durable pod cursor in older retained logs. Replaying them
		// would duplicate arbitrary output. Explicitly bound the gap instead.
		worker.cursor.at = worker.owner.resumedAt
		worker.discontinuity("controller adopted the workflow without a durable pod cursor; historical output is not replayed")
	}
	finalFailures := 0
	var finalCtx context.Context

	for {
		if worker.owner.ctx.Err() != nil {
			if worker.owner.drainExpired.Load() {
				worker.discontinuity("final log drain budget exhausted; retained output is preserved")
			}
			return
		}
		final := worker.terminal.Load()
		if final {
			worker.status("draining", "")
		}
		options := corev1.PodLogOptions{Container: worker.container, Follow: !final, Timestamps: true}
		if !worker.cursor.at.IsZero() {
			since := metav1.NewTime(worker.cursor.at.Truncate(time.Second))
			options.SinceTime = &since
		}
		parent := worker.owner.ctx
		if final {
			if finalCtx == nil {
				var cancelFinal context.CancelFunc
				finalCtx, cancelFinal = context.WithTimeout(parent, finalLogDrainBudget)
				defer cancelFinal()
			}
			parent = finalCtx
		}
		ctx, cancel := context.WithCancel(parent)
		lease := followLogIdleTimeout
		if final {
			lease = finalLogReadTimeout
		}
		timer := time.AfterFunc(lease, cancel)
		if !final {
			worker.followMu.Lock()
			worker.followCancel = cancel
			if worker.terminal.Load() {
				cancel()
			}
			worker.followMu.Unlock()
		}
		stream, err := worker.open(ctx, options)
		if err == nil {
			if !final {
				worker.status("following", "")
			}
			// A reader must honor cancellation while opening; closing it here also
			// releases Read implementations that only unblock when Close is called.
			stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
			var reader io.Reader = stream
			if !final {
				reader = &idleLogReader{Reader: stream, timer: timer, idle: lease}
			}
			err = worker.consumeRecords(reader, final)
			stop()
			_ = stream.Close()
		}
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		timer.Stop()
		cancel()
		worker.followMu.Lock()
		worker.followCancel = nil
		worker.followMu.Unlock()
		if worker.owner.failure() != nil {
			return
		}
		if errors.Is(err, errStepLogLimit) || errors.Is(err, errLogRecordLimit) {
			message := "step output exceeded the per-step byte ceiling"
			if errors.Is(err, errLogRecordLimit) {
				message = "pod log record exceeded the 1 MiB record ceiling"
			}
			worker.discontinuity(message)
			worker.status("truncated", message)
			return
		}
		if final && err != nil && finalFailures < 2 && finalCtx.Err() == nil {
			finalFailures++
			retry := time.NewTimer(time.Duration(finalFailures) * 250 * time.Millisecond)
			select {
			case <-finalCtx.Done():
				retry.Stop()
			case <-retry.C:
			}
			continue
		}
		if final {
			if err != nil {
				message := "final pod log read failed (" + logFailureClass(err) + "); retained output is preserved, final tail may be unavailable"
				worker.discontinuity(message)
				worker.status("unavailable", "final pod log read failed ("+logFailureClass(err)+")")
			} else if worker.gap {
				worker.status("gapped", "drain finished with a continuity gap")
			} else {
				worker.status("complete", "")
			}
			if err != nil || worker.owner.pod != nil {
				worker.reportUnknownTermination()
			}
			worker.complete = err == nil
			_ = worker.flush(true)
			return
		}
		worker.status("reconnecting", "pod stream ended; retrying from the last timestamp boundary")
		retryTimer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-worker.owner.ctx.Done():
			retryTimer.Stop()
			return
		case <-retryTimer.C:
		}
	}
}

// A terminal observation interrupts Follow immediately so an active producer
// cannot delay the bounded final snapshot indefinitely.
func (worker *stepLogFollower) markTerminal() {
	worker.terminal.Store(true)
	worker.followMu.Lock()
	defer worker.followMu.Unlock()
	if worker.followCancel != nil {
		worker.followCancel()
	}
}

// Progress is measured in bytes, including partial records: a long line must
// not lose its connection just because the scanner has not emitted it yet.
type idleLogReader struct {
	io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (reader *idleLogReader) Read(body []byte) (int, error) {
	n, err := reader.Reader.Read(body)
	if n > 0 {
		reader.timer.Reset(reader.idle)
	}
	return n, err
}

func (worker *stepLogFollower) open(ctx context.Context, options corev1.PodLogOptions) (io.ReadCloser, error) {
	identity := ""
	var diagnostic *model.StepDiagnostic
	node := worker.node
	if latest := worker.latestNode.Load(); latest != nil {
		node = *latest
	}
	if worker.owner.pod != nil {
		pod, err := worker.owner.pod(ctx, worker.namespace, worker.step.PodName)
		if err != nil {
			return nil, err
		}
		if !stepPodIdentityBound(worker.workflow, node, pod) {
			return nil, errLogIdentity
		}
		var status *corev1.ContainerStatus
		if init := failedInitContainer(pod); init != nil {
			status = init
		} else {
			for i := range pod.Status.ContainerStatuses {
				if pod.Status.ContainerStatuses[i].Name == mainContainerName {
					if status != nil {
						return nil, errLogIdentity
					}
					status = &pod.Status.ContainerStatuses[i]
				}
			}
		}
		if status == nil || status.ContainerID == "" {
			return nil, errLogIdentity
		}
		identity = fmt.Sprintf("%s/%s/%s/%d", pod.UID, status.Name, status.ContainerID, status.RestartCount)
		worker.container = status.Name
		diagnostic = projectStepDiagnostic(worker.workflow, node, pod, time.Now().UTC())
	} else if worker.owner.identity != nil {
		var err error
		identity, err = worker.owner.identity(ctx, worker.namespace, worker.step.PodName)
		if err != nil {
			return nil, err
		}
	}
	if identity != "" {
		if worker.identity != "" && worker.identity != identity {
			worker.discontinuity("pod or main container identity changed; continuity across container instances cannot be established")
			worker.cursor = logCursor{}
			worker.terminationReported = false
			worker.diagnostic = nil
			worker.skipBoundary = false
			options.SinceTime = nil
		}
		worker.identity = identity
	}
	if diagnostic != nil && diagnostic.Available {
		worker.diagnostic = diagnostic
		worker.reportTermination()
	}
	if worker.container != "" {
		options.Container = worker.container
	}
	return worker.owner.reader(ctx, worker.namespace, worker.step.PodName, options)
}

var errStepLogLimit = errors.New("step log byte ceiling reached")
var errLogRecordLimit = errors.New("pod log record byte ceiling reached")

// logCursor holds a count and rolling hash of the ordered records at the
// latest timestamp. Inclusive reconnects verify the whole boundary; identical
// repeated lines remain distinct. Cursor memory is constant even if millions
// of records share one timestamp.
type logCursor struct {
	at     time.Time
	count  int
	digest [32]byte
}

func logRecordDigest(previous, record [32]byte) [32]byte {
	var combined [64]byte
	copy(combined[:32], previous[:])
	copy(combined[32:], record[:])
	return sha256.Sum256(combined[:])
}

func (worker *stepLogFollower) consume(reader io.Reader) error {
	return worker.consumeRecords(reader, false)
}

// A final snapshot can legitimately end in an unterminated command line.
// Transport failures must never commit their incomplete frame.
type logRecordReader struct {
	io.Reader
	err        error
	beforeRead func() error
}

func (reader *logRecordReader) Read(body []byte) (int, error) {
	if reader.beforeRead != nil {
		if err := reader.beforeRead(); err != nil {
			reader.err = err
			return 0, err
		}
	}
	n, err := reader.Reader.Read(body)
	reader.err = err
	return n, err
}

func (worker *stepLogFollower) consumeRecords(reader io.Reader, final bool) (result error) {
	defer func() { result = errors.Join(result, worker.flush(false)) }()
	source := &logRecordReader{Reader: reader, beforeRead: func() error { return worker.flush(false) }}
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 4096), maxLogRecordBytes+1)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i], nil
		}
		if atEOF && len(data) > 0 {
			if final && errors.Is(source.err, io.EOF) {
				return len(data), data, nil
			}
			return 0, nil, errors.New("incomplete Kubernetes log record")
		}
		return 0, nil, nil
	})
	boundary := worker.cursor.at
	expectedCount, expectedDigest := worker.cursor.count, worker.cursor.digest
	var replayDigest [32]byte
	seen := 0
	checked := boundary.IsZero() || expectedCount == 0
	for scanner.Scan() {
		record := scanner.Text()
		stamp, text, ok := strings.Cut(record, " ")
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if !ok || err != nil {
			return errors.New("pod stream lacks valid Kubernetes timestamps")
		}
		if at.Before(boundary) {
			continue
		}
		digest := sha256.Sum256([]byte(text))
		if worker.skipBoundary && at.Equal(boundary) {
			continue
		}
		if at.Equal(boundary) && seen < expectedCount {
			replayDigest = logRecordDigest(replayDigest, digest)
			seen++
			if seen == expectedCount {
				checked = true
				if replayDigest != expectedDigest {
					worker.discontinuity("timestamp boundary changed or rotated; remaining records at this timestamp cannot be reconciled")
					worker.skipBoundary = true
				}
			}
			continue
		}
		if !checked {
			worker.discontinuity("last timestamp boundary is missing; pod logs may have rotated")
			checked = true
		}
		prefix := "[" + worker.step.Burn + "/" + worker.step.Step + "] "
		body := []byte(prefix + text + "\n")
		if hint := worker.proxyDiagnostic.observe(text, worker.owner.goProxyURL); hint != "" {
			body = append(body, []byte(prefix+hint+"\n")...)
		}
		if worker.bytes+int64(len(body)) > maxStepLogBytes {
			return errStepLogLimit
		}
		if len(worker.pending)+len(body) > (1<<20)+8192 {
			if err := worker.flush(false); err != nil {
				return err
			}
		}
		worker.pending = append(worker.pending, body...)
		worker.bytes += int64(len(body))
		if !at.Equal(worker.cursor.at) {
			worker.cursor.at = at
			worker.cursor.count = 0
			worker.cursor.digest = [32]byte{}
			worker.skipBoundary = false
		}
		worker.cursor.count++
		worker.cursor.digest = logRecordDigest(worker.cursor.digest, digest)
		if len(worker.pending) >= 32<<10 {
			if err := worker.flush(false); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return errLogRecordLimit
		}
		return err
	}
	if !checked {
		worker.discontinuity("pod stream ended before the retained timestamp boundary was recovered")
	}
	return nil
}

func (worker *stepLogFollower) checkpoint() runlog.PodLogCursor {
	uid := ""
	if worker.workflow != nil {
		uid = string(worker.workflow.UID)
	}
	return runlog.PodLogCursor{Key: worker.key, WorkflowUID: uid, Node: worker.step.NodeID, Pod: worker.step.PodName, Container: worker.container, Identity: worker.identity, At: worker.cursor.at, Count: worker.cursor.count, Digest: worker.cursor.digest, Bytes: worker.bytes, Gap: worker.gap, SkipBoundary: worker.skipBoundary, Complete: worker.complete, Termination: worker.diagnostic, TerminationReported: worker.terminationReported}
}
func (worker *stepLogFollower) flush(force bool) error {
	if len(worker.pending) == 0 && !force {
		return nil
	}
	var err error
	if sink, ok := worker.owner.output.(runlog.PodLogSink); ok && worker.key != "" {
		err = sink.CommitPodLog(worker.checkpoint(), worker.pending)
	} else if len(worker.pending) > 0 {
		_, err = worker.owner.output.Write(worker.pending)
	}
	if err != nil {
		worker.owner.mu.Lock()
		worker.owner.err = errors.Join(worker.owner.err, err)
		worker.owner.mu.Unlock()
		return err
	}
	worker.pending = nil
	return nil
}
func logFailureClass(err error) string {
	var netErr net.Error
	switch {
	case apierrors.IsNotFound(err):
		return "pod-not-found"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return "deadline-or-cancellation"
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return "access-refused"
	case apierrors.IsTooManyRequests(err):
		return "api-throttled"
	case apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err):
		return "api-timeout"
	case apierrors.IsInternalError(err) || apierrors.IsServiceUnavailable(err):
		return "api-unavailable"
	case errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF):
		return "transport"
	case errors.As(err, &netErr):
		return "transport"
	case errors.Is(err, errLogIdentity):
		return "container-identity-unavailable"
	default:
		return "log-endpoint-or-record-error"
	}
}

var errLogIdentity = errors.New("container identity unavailable")

func (worker *stepLogFollower) reportTermination() {
	if worker.terminationReported || worker.diagnostic == nil || !worker.diagnostic.Available {
		return
	}
	c := worker.diagnostic.Container
	label := "container"
	if c.ContainerKind == "init" {
		label = "init container"
	}
	body := "[" + worker.step.Burn + "/" + worker.step.Step + "] oberth: " + label + " " + c.ContainerName + " terminated: exit code " + strconv.Itoa(int(c.ExitCode)) + ", reason " + c.Reason
	if c.Message != "" {
		body += ", message " + strconv.Quote(c.Message)
	}
	if c.MessageTruncated {
		body += " (message truncated)"
	}
	worker.pending = append(worker.pending, body+"\n"...)
	worker.terminationReported = true
	_ = worker.flush(true)
}
func (worker *stepLogFollower) reportUnknownTermination() {
	if worker.terminationReported {
		return
	}
	node := worker.node
	if latest := worker.latestNode.Load(); latest != nil {
		node = *latest
	}
	message := "container termination unavailable; no observed exit code or termination message"
	if node.Outputs != nil && node.Outputs.ExitCode != nil {
		if code, err := parseExitCode(*node.Outputs.ExitCode); err == nil {
			message = "Argo node exit code " + strconv.Itoa(code) + "; Kubernetes container termination message unavailable"
		}
	}
	worker.write([]byte("[" + worker.step.Burn + "/" + worker.step.Step + "] oberth: " + message + "\n"))
}

// Prefer actual observed termination over a later unavailable Pod snapshot.
// Workers are joined before this call; durable cursors also cover adoption.
func (logs *stepLogFollowers) applyDiagnostics(completion *Completion) {
	for i := range completion.Steps {
		step := &completion.Steps[i]
		worker := logs.workers[step.NodeID+"/"+step.PodName]
		if worker == nil || worker.diagnostic == nil || !worker.diagnostic.Available {
			continue
		}
		step.Termination = worker.diagnostic
		if worker.diagnostic.Container.ContainerKind == "init" {
			step.ExitCode = int(worker.diagnostic.Container.ExitCode)
		}
	}
}
