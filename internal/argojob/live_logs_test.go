package argojob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
)

const logStamp = "2026-10-23T01:00:00.123456789Z"

func stamped(lines ...string) string {
	return logStamp + " " + strings.Join(lines, "\n"+logStamp+" ") + "\n"
}

func testLogWorker(output io.Writer) *stepLogFollower {
	owner := newStepLogFollowers(context.Background(), nil, output, nil)
	return &stepLogFollower{owner: owner, step: StepResult{Burn: "build", Step: "compile"}}
}

func TestLiveCursorInclusiveRepeatedTimestamp(t *testing.T) {
	var output bytes.Buffer
	worker := testLogWorker(&output)
	for _, batch := range []string{stamped("one", "repeat", "repeat"), stamped("one", "repeat", "repeat", "repeat", "two"), stamped("one", "repeat", "repeat", "repeat", "two")} {
		if err := worker.consume(strings.NewReader(batch)); err != nil {
			t.Fatal(err)
		}
	}
	want := "[build/compile] one\n" + strings.Repeat("[build/compile] repeat\n", 3) + "[build/compile] two\n"
	if output.String() != want {
		t.Fatalf("got %q, want %q", output.String(), want)
	}
}

type droppedLog struct{ body string }

func (r *droppedLog) Read(p []byte) (int, error) {
	if r.body == "" {
		return 0, errors.New("injected dropped stream")
	}
	n := copy(p, r.body)
	r.body = r.body[n:]
	return n, nil
}
func TestLiveCursorDroppedPartialAndRotatedBoundary(t *testing.T) {
	var output bytes.Buffer
	worker := testLogWorker(&output)
	if err := worker.consume(&droppedLog{stamped("one") + logStamp + " par"}); err == nil {
		t.Fatal("expected read failure")
	}
	if err := worker.consume(strings.NewReader(stamped("one", "partial"))); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "] par\n") || strings.Count(output.String(), "] one\n") != 1 || strings.Count(output.String(), "] partial\n") != 1 {
		t.Fatal(output.String())
	}
	if err := worker.consume(strings.NewReader(stamped("rotated") + "2026-10-23T01:00:01Z tail\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "log continuity gap") || strings.Count(output.String(), "] tail\n") != 1 {
		t.Fatal(output.String())
	}
}

func TestLiveLogInterleavingIndexesAndBudgets(t *testing.T) {
	store, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("interleaved")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	budget := &runBudgetWriter{destination: file, remaining: 1 << 20}
	var wg sync.WaitGroup
	for _, key := range []string{"compile", "test"} {
		wg.Go(func() {
			worker := testLogWorker(budget)
			worker.step.Step = key
			// Every transport read can cut inside a line; no partial line reaches the
			// writer shared with other steps or progress markers.
			pipeR, pipeW := io.Pipe()
			go func() {
				defer pipeW.Close()
				for i := 0; i < 50; i++ {
					body := fmt.Sprintf("%s line-%03d\n", logStamp, i)
					for j := 0; j < len(body); j += 3 {
						_, _ = pipeW.Write([]byte(body[j:min(j+3, len(body))]))
					}
				}
			}()
			if err := worker.consume(pipeR); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for _, key := range []string{"compile", "test"} {
		active, err := store.ReadActive("interleaved", "build", key)
		if err != nil {
			t.Fatal(err)
		}
		var want strings.Builder
		for i := 0; i < 50; i++ {
			fmt.Fprintf(&want, "[build/%s] line-%03d\n", key, i)
		}
		if string(active) != want.String() {
			t.Fatalf("%s attribution/order lost", key)
		}
	}
	if _, err := store.BuildIndex("interleaved"); err != nil {
		t.Fatal(err)
	}
	body, err := store.Read("interleaved", "build", "compile")
	if err != nil || bytes.Count(body, []byte("line-")) != 50 {
		t.Fatalf("terminal index: %s %v", body, err)
	}
	small := testLogWorker(&runBudgetWriter{destination: io.Discard, remaining: 2})
	if err := small.consume(strings.NewReader(stamped("too big"))); !errors.Is(err, errRunLogBudget) {
		t.Fatalf("budget error: %v", err)
	}
	limited := testLogWorker(io.Discard)
	limited.bytes = maxStepLogBytes
	if err := limited.consume(strings.NewReader(stamped("too big"))); !errors.Is(err, errStepLogLimit) {
		t.Fatalf("step limit: %v", err)
	}
}

type liveWorkflowClient struct {
	staticWorkflowClient
	finished atomic.Bool
	pending  bool
}

func (c *liveWorkflowClient) Get(context.Context, string, metav1.GetOptions) (*wfv1.Workflow, error) {
	wf := c.workflow.DeepCopy()
	if !c.finished.Load() {
		wf.Status.Phase = wfv1.WorkflowRunning
		for key, node := range wf.Status.Nodes {
			if node.Type == wfv1.NodeTypePod {
				node.Phase = wfv1.NodeRunning
				if c.pending {
					node.Phase = wfv1.NodePending
				}
				node.FinishedAt = metav1.Time{}
				wf.Status.Nodes[key] = node
			}
		}
	}
	return wf, nil
}

func TestWaitLiveBeforeCompletionAndFinalTail(t *testing.T) {
	store, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := store.Create("live")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	client := &liveWorkflowClient{staticWorkflowClient: staticWorkflowClient{workflow: singleStepWorkflow()}, pending: true}
	controller, err := NewController(client, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	controller.interval = 5 * time.Millisecond
	opened := make(chan struct{})
	var once sync.Once
	var finalCalls atomic.Int32
	controller.WithPodLogFollower(func(ctx context.Context, _, _ string, options corev1.PodLogOptions) (io.ReadCloser, error) {
		if options.Container != "main" || !options.Timestamps {
			t.Error("missing timestamped main container request")
		}
		if !options.Follow {
			if options.SinceTime == nil {
				t.Error("missing reconnect cursor")
			}
			if finalCalls.Add(1) == 1 {
				return nil, errors.New("transient final read failure")
			}
			return io.NopCloser(strings.NewReader(stamped("line-001", "identical", "identical", "final-tail"))), nil
		}
		reader, writer := io.Pipe()
		go func() {
			_, _ = io.WriteString(writer, stamped("line-001", "identical", "identical"))
			once.Do(func() { close(opened) })
			<-ctx.Done()
			_ = writer.CloseWithError(ctx.Err())
		}()
		return reader, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		completion, err := controller.Wait(ctx, "wf", "run-single", file)
		if err == nil && !completion.Succeeded {
			err = fmt.Errorf("verdict %s", completion.Phase)
		}
		result <- err
	}()
	select {
	case <-opened:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// A real growing file and active step reader observe the numbered line while
	// the workflow is still running, independently of the browser connection.
	deadline := time.Now().Add(time.Second)
	for {
		body, readErr := store.ReadActive("live", "build", "compile")
		if readErr == nil && strings.Contains(string(body), "line-001") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not delivered before completion: %s %v", body, readErr)
		}
		time.Sleep(time.Millisecond)
	}
	client.finished.Store(true)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"line-001", "final-tail"} {
		if bytes.Count(body, []byte("] "+line+"\n")) != 1 {
			t.Fatalf("%s missing or duplicated: %s", line, body)
		}
	}
	if bytes.Count(body, []byte("] identical\n")) != 2 {
		t.Fatal(string(body))
	}
	if !bytes.Contains(body, []byte(`"state":"complete"`)) {
		t.Fatal("final delivery not complete")
	}
}

func TestLiveCancellationAndAdoptionGap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	opened := make(chan struct{})
	logs := newStepLogFollowers(ctx, func(ctx context.Context, _, _ string, _ corev1.PodLogOptions) (io.ReadCloser, error) {
		close(opened)
		reader, writer := io.Pipe()
		go func() { <-ctx.Done(); _ = writer.CloseWithError(ctx.Err()) }()
		return reader, nil
	}, io.Discard, nil)
	wf := singleStepWorkflow()
	wf.Status.Phase = wfv1.WorkflowRunning
	for key, node := range wf.Status.Nodes {
		if node.Type == wfv1.NodeTypePod {
			node.Phase = wfv1.NodeRunning
			wf.Status.Nodes[key] = node
		}
	}
	logs.observe(wf)
	<-opened
	cancel()
	logs.close()
	var output bytes.Buffer
	var since *metav1.Time
	restored := newStepLogFollowers(context.Background(), func(_ context.Context, _, _ string, options corev1.PodLogOptions) (io.ReadCloser, error) {
		since = options.SinceTime
		return io.NopCloser(strings.NewReader(stamped("historical"))), nil
	}, &runBudgetWriter{destination: &output, remaining: 1 << 20}, []runprogress.Event{{Burn: "build", Step: "compile", Status: runprogress.StepRunning}})
	restored.resumedAt, _ = time.Parse(time.RFC3339Nano, "2026-10-24T01:00:00Z")
	restored.drain(singleStepWorkflow())
	restored.close()
	if since == nil || !strings.Contains(output.String(), "log continuity gap") || strings.Contains(output.String(), "] historical\n") {
		t.Fatal(output.String())
	}
}

func TestLiveFinalUnterminatedLine(t *testing.T) {
	var output bytes.Buffer
	worker := testLogWorker(&output)
	if err := worker.consumeRecords(strings.NewReader(stamped("head")+logStamp+" tail"), true); err != nil {
		t.Fatal(err)
	}
	if err := worker.consumeRecords(strings.NewReader(stamped("head")+logStamp+" tail"), true); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), "] tail\n") != 1 {
		t.Fatal(output.String())
	}
	dropped := testLogWorker(io.Discard)
	if err := dropped.consumeRecords(&droppedLog{logStamp + " incomplete"}, true); err == nil {
		t.Fatal("committed a transport-truncated final record")
	}
}

func TestLiveContainerIdentityChange(t *testing.T) {
	var output bytes.Buffer
	worker := testLogWorker(&output)
	worker.identity = "pod-uid/container-old/0"
	worker.owner.identity = func(context.Context, string, string) (string, error) { return "pod-uid/container-new/1", nil }
	var requestedSince *metav1.Time
	worker.owner.reader = func(_ context.Context, _, _ string, options corev1.PodLogOptions) (io.ReadCloser, error) {
		requestedSince = options.SinceTime
		return io.NopCloser(strings.NewReader(stamped("same-text"))), nil
	}
	if err := worker.consume(strings.NewReader(stamped("same-text"))); err != nil {
		t.Fatal(err)
	}
	since := metav1.Now()
	stream, err := worker.open(context.Background(), corev1.PodLogOptions{SinceTime: &since})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := worker.consumeRecords(stream, true); err != nil {
		t.Fatal(err)
	}
	if requestedSince != nil || strings.Count(output.String(), "] same-text\n") != 2 || !strings.Contains(output.String(), "container identity changed") {
		t.Fatal(output.String())
	}
}

func TestLiveFollowerConcurrencyBoundAndFastSteps(t *testing.T) {
	wf := singleStepWorkflow()
	prototype := wf.Status.Nodes["compile-pod"]
	delete(wf.Status.Nodes, "compile-pod")
	total := maxLogFollowers + 7
	for i := 0; i < total; i++ {
		node := prototype
		node.ID = fmt.Sprintf("pod-%d", i)
		node.Name = fmt.Sprintf("wf.build.step-%d", i)
		node.DisplayName = fmt.Sprintf("step-%d", i)
		wf.Status.Nodes[node.ID] = node
	}
	var active, peak, calls atomic.Int32
	ready := make(chan struct{}, maxLogFollowers)
	release := make(chan struct{})
	logs := newStepLogFollowers(context.Background(), func(_ context.Context, _, _ string, options corev1.PodLogOptions) (io.ReadCloser, error) {
		if options.Follow {
			t.Error("fast terminal step requested Follow")
		}
		current := active.Add(1)
		defer active.Add(-1)
		calls.Add(1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		if current <= maxLogFollowers {
			select {
			case ready <- struct{}{}:
			default:
			}
		}
		<-release
		return io.NopCloser(strings.NewReader(stamped("fast"))), nil
	}, &runBudgetWriter{destination: io.Discard, remaining: 1 << 20}, nil)
	logs.observe(wf)
	deadline := time.After(5 * time.Second)
	for i := 0; i < maxLogFollowers; i++ {
		select {
		case <-ready:
		case <-deadline:
			close(release)
			logs.close()
			t.Fatal("workers did not start")
		}
	}
	if peak.Load() > maxLogFollowers {
		t.Fatalf("unbounded streams: %d", peak.Load())
	}
	close(release)
	logs.drain(wf)
	logs.close()
	if int(calls.Load()) != total {
		t.Fatalf("fast steps lost: got %d calls, want %d", calls.Load(), total)
	}
}

func TestLiveUnavailableLogsDoNotChangeCommandVerdict(t *testing.T) {
	wf := singleStepWorkflow()
	controller, err := NewController(&staticWorkflowClient{workflow: wf}, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	controller.WithPodLogFollower(func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
		return nil, errors.New("injected log outage")
	})
	var output bytes.Buffer
	completion, err := controller.Wait(context.Background(), wf.Name, "run-single", &output)
	if err != nil || !completion.Succeeded {
		t.Fatalf("log outage changed command verdict: %+v %v", completion, err)
	}
	if !strings.Contains(output.String(), `"state":"unavailable"`) || !strings.Contains(output.String(), "log continuity gap") {
		t.Fatal(output.String())
	}
}

func TestLiveOversizedRecordIsBounded(t *testing.T) {
	worker := testLogWorker(io.Discard)
	body := logStamp + " " + strings.Repeat("x", maxLogRecordBytes) + "\n"
	if err := worker.consumeRecords(strings.NewReader(body), true); !errors.Is(err, errLogRecordLimit) {
		t.Fatalf("record ceiling: %v", err)
	}
}
