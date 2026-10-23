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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/runprogress"
)

// PodLogFollower must honor cancellation and return Kubernetes timestamped
// records. SinceTime is inclusive; a timestamp alone is not a unique cursor.
type PodLogFollower func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error)

func (controller *Controller) WithPodLogFollower(reader PodLogFollower) *Controller {
	controller.followLogs = reader
	controller.logIdentity = nil
	return controller
}

type podLogIdentityReader func(context.Context, string, string) (string, error)

const (
	maxLogFollowers   = 32
	maxLogRecordBytes = 1 << 20
)

// Only complete attributed lines reach the shared, serialized budget writer.
// No pod can interleave a partial line with another pod or a progress marker.
// A bounded read lease also prevents a stalled connection blocking final drain.
type stepLogFollowers struct {
	ctx       context.Context
	cancel    context.CancelFunc
	reader    PodLogFollower
	identity  podLogIdentityReader
	output    io.Writer
	workers   map[string]*stepLogFollower
	resumed   map[string]runprogress.Status
	resumedAt time.Time
	mu        sync.Mutex
	err       error
}

type stepLogFollower struct {
	owner        *stepLogFollowers
	step         StepResult
	namespace    string
	terminal     atomic.Bool
	done         chan struct{}
	cursor       logCursor
	bytes        int64
	state        string
	stateMessage string
	lastGap      string
	gap          bool
	skipBoundary bool
	identity     string
}

func newStepLogFollowers(ctx context.Context, reader PodLogFollower, output io.Writer, events []runprogress.Event) *stepLogFollowers {
	ctx, cancel := context.WithCancel(ctx)
	resumed := map[string]runprogress.Status{}
	for _, event := range events {
		resumed[event.Burn+"/"+event.Step] = event.Status
	}
	return &stepLogFollowers{ctx: ctx, cancel: cancel, reader: reader, output: output, workers: map[string]*stepLogFollower{}, resumed: resumed, resumedAt: time.Now().UTC()}
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
				worker.terminal.Store(true)
			}
			continue
		}
		// Overflow stays eligible on the next observation and is always collected
		// during drain. Do not allocate one goroutine or buffer per pending leaf.
		if active >= maxLogFollowers || busySteps[step.Burn+"/"+step.Step] {
			continue
		}
		worker := &stepLogFollower{owner: logs, step: step, namespace: workflow.Namespace, done: make(chan struct{})}
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
	// Finished-between-observations steps and overflow are drained in bounded
	// batches. A pod log error degrades delivery, never the command verdict.
	for {
		logs.observe(workflow)
		for _, worker := range logs.workers {
			worker.terminal.Store(true)
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

	if worker.step.Attempt != "" {
		worker.write([]byte("[" + worker.step.Burn + "/" + worker.step.Step + "] oberth: Argo retry attempt " + worker.step.Attempt + "\n"))
	}
	if previous, restored := worker.owner.resumed[worker.step.Burn+"/"+worker.step.Step]; restored && (worker.step.StartedAt == nil || worker.step.StartedAt.Before(worker.owner.resumedAt)) {
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
	for {
		if worker.owner.ctx.Err() != nil {
			return
		}
		final := worker.terminal.Load()
		if final {
			worker.status("draining", "")
		}
		options := corev1.PodLogOptions{Container: mainContainerName, Follow: !final, Timestamps: true}
		if !worker.cursor.at.IsZero() {
			since := metav1.NewTime(worker.cursor.at.Truncate(time.Second))
			options.SinceTime = &since
		}
		lease := 3 * time.Second
		if final {
			lease = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(worker.owner.ctx, lease)
		stream, err := worker.open(ctx, options)
		if err == nil {
			if !final {
				worker.status("following", "")
			}
			// A reader must honor cancellation while opening; closing it here also
			// releases Read implementations that only unblock when Close is called.
			stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
			err = worker.consumeRecords(stream, final)
			stop()
			_ = stream.Close()
		}
		cancel()
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
		if final && err != nil && finalFailures < 2 {
			finalFailures++
			continue
		}
		if final {
			if err != nil {
				worker.discontinuity("final pod log read failed; final tail may be unavailable")
				worker.status("unavailable", "final pod log read failed")
			} else if worker.gap {
				worker.status("gapped", "drain finished with a continuity gap")
			} else {
				worker.status("complete", "")
			}
			return
		}
		worker.status("reconnecting", "pod stream ended; retrying from the last timestamp boundary")
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-worker.owner.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (worker *stepLogFollower) open(ctx context.Context, options corev1.PodLogOptions) (io.ReadCloser, error) {
	if worker.owner.identity != nil {
		identity, err := worker.owner.identity(ctx, worker.namespace, worker.step.PodName)
		if err != nil {
			return nil, err
		}
		if worker.identity != "" && worker.identity != identity {
			worker.discontinuity("pod or main container identity changed; continuity across container instances cannot be established")
			worker.cursor = logCursor{}
			worker.skipBoundary = false
			options.SinceTime = nil
		}
		worker.identity = identity
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
	err error
}

func (reader *logRecordReader) Read(body []byte) (int, error) {
	n, err := reader.Reader.Read(body)
	reader.err = err
	return n, err
}

func (worker *stepLogFollower) consumeRecords(reader io.Reader, final bool) error {
	source := &logRecordReader{Reader: reader}
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
		body := []byte("[" + worker.step.Burn + "/" + worker.step.Step + "] " + text + "\n")
		if worker.bytes+int64(len(body)) > maxStepLogBytes {
			return errStepLogLimit
		}
		if !worker.write(body) {
			return worker.owner.failure()
		}
		worker.bytes += int64(len(body))
		if !at.Equal(worker.cursor.at) {
			worker.cursor.at = at
			worker.cursor.count = 0
			worker.cursor.digest = [32]byte{}
			worker.skipBoundary = false
		}
		worker.cursor.count++
		worker.cursor.digest = logRecordDigest(worker.cursor.digest, digest)
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
