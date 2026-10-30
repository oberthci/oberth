package argojob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
)

type durableTestLog struct {
	file    *os.File
	journal *runlog.PodLogJournal
}

func (s *durableTestLog) Write(b []byte) (int, error) { return s.file.Write(b) }
func (s *durableTestLog) CommitPodLog(c runlog.PodLogCursor, b []byte) error {
	return s.journal.Commit(c, b, s.file.Write)
}
func (s *durableTestLog) PodLogCursors() map[string]runlog.PodLogCursor { return s.journal.Cursors() }
func durableLogFixture(t *testing.T) *durableTestLog {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "run.log"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	j, err := runlog.OpenPodLogJournal(f, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close(); _ = f.Close() })
	return &durableTestLog{f, j}
}
func TestContinuityAdoptsDurableCursorAndRepeatedRecords(t *testing.T) {
	sink := durableLogFixture(t)
	wf := singleStepWorkflow()
	wf.UID = "workflow-uid"
	step := stepAttempts(wf)[0]
	key := string(wf.UID) + "/" + step.NodeID + "/" + step.PodName
	first := testLogWorker(&runBudgetWriter{destination: sink, remaining: 1 << 20})
	first.step = step
	first.workflow = wf
	first.key = key
	first.container = "main"
	if err := first.consume(strings.NewReader(stamped("before", "repeat", "repeat"))); err != nil {
		t.Fatal(err)
	}
	// Actual restart: close/reopen the journal, rather than reusing an in-memory cursor.
	_ = sink.journal.Close()
	j, err := runlog.OpenPodLogJournal(sink.file, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sink.journal = j
	defer j.Close()
	events := []runprogress.Event{{Burn: step.Burn, Step: step.Step, Status: runprogress.StepPassed}}
	reads := 0
	logs := newStepLogFollowers(context.Background(), func(_ context.Context, _, _ string, o corev1.PodLogOptions) (io.ReadCloser, error) {
		reads++
		if o.SinceTime == nil {
			t.Error("adopted cursor missing")
		}
		return io.NopCloser(strings.NewReader(stamped("before", "repeat", "repeat", "repeat", "after"))), nil
	}, &runBudgetWriter{destination: sink, remaining: 1 << 20}, events)
	logs.drain(wf)
	logs.close()
	raw, _ := os.ReadFile(sink.file.Name())
	if reads != 1 || bytes.Count(raw, []byte("] before\n")) != 1 || bytes.Count(raw, []byte("] repeat\n")) != 3 || bytes.Count(raw, []byte("] after\n")) != 1 || bytes.Contains(raw, []byte("historical output is not replayed")) {
		t.Fatalf("replayed/missing output (%d): %s", reads, raw)
	}
	// A complete checkpoint never fetches or duplicates a terminal Pod on a later adoption.
	again := newStepLogFollowers(context.Background(), func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
		t.Error("complete cursor fetched again")
		return nil, errors.New("Pod GCd")
	}, &runBudgetWriter{destination: sink, remaining: 1 << 20}, events)
	again.drain(wf)
	again.close()
	final, _ := os.ReadFile(sink.file.Name())
	if !bytes.Equal(raw, final) {
		t.Fatal("terminal adoption changed retained bytes")
	}
}
func TestContinuityKeepsStreamedLinesWhenPodGone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output bytes.Buffer
		wf := singleStepWorkflow()
		step := stepAttempts(wf)[0]
		logs := newStepLogFollowers(context.Background(), func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
			return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, step.PodName)
		}, &runBudgetWriter{destination: &output, remaining: 1 << 20}, nil)
		worker := &stepLogFollower{owner: logs, step: step, done: make(chan struct{})}
		if err := worker.consume(strings.NewReader(stamped("already received"))); err != nil {
			t.Fatal(err)
		}
		worker.markTerminal()
		worker.run()
		if strings.Count(output.String(), "] already received\n") != 1 || !strings.Contains(output.String(), "pod-not-found") || !strings.Contains(output.String(), "no observed exit code") || strings.Contains(output.String(), "exit code 1") {
			t.Fatal(output.String())
		}
	})
}
func TestContinuitySlowFinalEndpointAndSharedDrainBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output bytes.Buffer
		worker := testLogWorker(&output)
		worker.done = make(chan struct{})
		worker.markTerminal()
		worker.owner.reader = func(ctx context.Context, _, _ string, _ corev1.PodLogOptions) (io.ReadCloser, error) {
			select {
			case <-time.After(45 * time.Second):
				return io.NopCloser(strings.NewReader(stamped("slow real tail"))), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		worker.run()
		if !strings.Contains(output.String(), "] slow real tail\n") || !strings.Contains(output.String(), `"state":"complete"`) {
			t.Fatal(output.String())
		}
	})
	synctest.Test(t, func(t *testing.T) {
		wf := singleStepWorkflow()
		node := wf.Status.Nodes["compile-pod"]
		delete(wf.Status.Nodes, "compile-pod")
		for i := 0; i < maxLogFollowers+3; i++ {
			n := node
			n.ID = node.ID + strings.Repeat("x", i+1)
			n.Name = node.Name + strings.Repeat("x", i+1)
			n.DisplayName = n.Name
			wf.Status.Nodes[n.ID] = n
		}
		logs := newStepLogFollowers(context.Background(), func(ctx context.Context, _, _ string, _ corev1.PodLogOptions) (io.ReadCloser, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, &runBudgetWriter{destination: io.Discard, remaining: 1 << 20}, nil)
		start := time.Now()
		logs.drain(wf)
		logs.close()
		if time.Since(start) > finalLogDrainBudget {
			t.Fatal("overflow extended total final-drain budget", time.Since(start))
		}
	})
}
func TestContinuityFailedInitLogsExitAndMessage(t *testing.T) {
	wf, node, pod, _ := diagnosticFixture()
	node.Outputs = nil
	wf.Status.Nodes[node.ID] = node
	term := pod.Status.ContainerStatuses[0].State.Terminated.DeepCopy()
	term.ExitCode = 23
	term.Reason = "Error"
	term.Message = "Failed to open /tmp/tools/jq: Read-only file system\n[forged/step] x"
	pod.Status.ContainerStatuses[0] = corev1.ContainerStatus{Name: "main", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}
	pod.Spec.InitContainers = []corev1.Container{{Name: "install-jq"}}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "install-jq", ContainerID: term.ContainerID, State: corev1.ContainerState{Terminated: term}}}
	var output bytes.Buffer
	var mu sync.Mutex
	calls := 0
	controller, err := NewController(&staticWorkflowClient{workflow: wf}, nil, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	controller.WithPodLogFollower(func(_ context.Context, _, _ string, o corev1.PodLogOptions) (io.ReadCloser, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		if o.Container != "install-jq" {
			t.Errorf("selected %q instead of failed init", o.Container)
		}
		return io.NopCloser(strings.NewReader(stamped("curl: Failed to open the file: Read-only file system"))), nil
	})
	controller.logPod = func(context.Context, string, string) (*corev1.Pod, error) { return pod.DeepCopy(), nil }
	var completion Completion
	completion, err = controller.Wait(context.Background(), wf.Name, strings.Repeat("a", 32), &output)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(completion.Steps) != 1 || completion.Steps[0].ExitCode != 23 || completion.Steps[0].Termination == nil || !completion.Steps[0].Termination.Available || completion.Steps[0].Termination.Container.ContainerKind != "init" {
		t.Fatalf("init evidence lost: %+v reads=%d", completion, calls)
	}
	if !strings.Contains(output.String(), "init container install-jq terminated: exit code 23, reason Error") || !strings.Contains(output.String(), "curl: Failed to open") || strings.Contains(output.String(), "\n[forged/step]") {
		t.Fatal(output.String())
	}
	// A foreign-owned Pod must never supply a log or termination observation.
	pod.OwnerReferences[0].UID = "foreign"
	worker := testLogWorker(io.Discard)
	worker.workflow = wf
	worker.node = node
	worker.namespace = wf.Namespace
	worker.step = completion.Steps[0]
	worker.owner.pod = controller.logPod
	if _, err := worker.open(context.Background(), corev1.PodLogOptions{}); !errors.Is(err, errLogIdentity) {
		t.Fatal("foreign Pod accepted", err)
	}
}
func TestContinuityTerminationMessageBoundAndInitMainFence(t *testing.T) {
	wf, node, pod, at := diagnosticFixture()
	pod.Status.ContainerStatuses[0].State.Terminated.Message = strings.Repeat("\x00💥", 2000)
	d := projectStepDiagnostic(wf, node, pod, at)
	raw, _ := json.Marshal(d)
	if !d.Available || d.Validate() != nil || !d.Container.MessageTruncated || len(raw) > 4096 {
		t.Fatalf("unbounded termination %+v (%d bytes)", d, len(raw))
	}
	init := pod.Status.ContainerStatuses[0].DeepCopy()
	init.Name = "init-failed"
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{*init}
	if failedInitContainer(pod) != nil {
		t.Fatal("failed init displaced main that actually ran")
	}
}
func TestContinuityWorkflowRetentionExceedsFinalDrain(t *testing.T) {
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{PodGC: &wfv1.PodGC{Strategy: wfv1.PodGCOnPodCompletion}}}
	cfg := testConfig()
	cfg.TTLSeconds = 1
	applyServerBounds(wf, cfg)
	if wf.Spec.PodGC != nil || time.Duration(*wf.Spec.TTLStrategy.SecondsAfterCompletion)*time.Second < finalLogDrainBudget+30*time.Second {
		t.Fatal("Pod retention shorter than bounded final read")
	}
	if *wf.Spec.TTLStrategy.SecondsAfterCompletion != 180 {
		t.Fatal("unexpected retention floor")
	}
	// An admin's longer bounded ownership window survives unchanged.
	cfg.TTLSeconds = 3600
	applyServerBounds(wf, cfg)
	if *wf.Spec.TTLStrategy.SecondsAfterCompletion != 3600 {
		t.Fatal("configured retention lost")
	}
}

func TestContinuityProvidedInitErrorStatusDumps(t *testing.T) {
	raw, err := os.ReadFile("testdata/log-continuity/init-error.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Workflow wfv1.Workflow `json:"workflow"`
		Pod      corev1.Pod    `json:"pod"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range cases {
		t.Run(fixture.Workflow.Name, func(t *testing.T) {
			wf, pod := &fixture.Workflow, &fixture.Pod
			for _, status := range pod.Status.InitContainerStatuses {
				pod.Spec.InitContainers = append(pod.Spec.InitContainers, corev1.Container{Name: status.Name})
			}
			node := wf.Status.Nodes[pod.Annotations["workflows.argoproj.io/node-id"]]
			d := projectStepDiagnostic(wf, node, pod, time.Now().UTC())
			if !d.Available || d.Container.ContainerKind != "init" || d.Container.ContainerName != "install-jq" || d.Container.ExitCode != 23 || d.Container.Reason != "Error" || d.Container.Message != "" {
				t.Fatalf("failed init not projected: %+v", d)
			}
			timing := projectStepTiming(wf, node, pod, StepResult{NodeID: node.ID, PodName: pod.Name, Burn: "test", Step: "stock-cos"}, d, time.Now().UTC())
			if timing.Main != nil || timing.Available || len(timing.Init) != 3 || timing.Validate() != nil {
				t.Fatalf("init mislabeled as main timing: %+v", timing)
			}
			worker := testLogWorker(io.Discard)
			worker.workflow = wf
			worker.node = node
			worker.namespace = wf.Namespace
			worker.step.PodName = pod.Name
			worker.owner.pod = func(context.Context, string, string) (*corev1.Pod, error) { return pod, nil }
			worker.owner.reader = func(_ context.Context, _, _ string, o corev1.PodLogOptions) (io.ReadCloser, error) {
				if o.Container != "install-jq" {
					t.Fatalf("main-only collector regression: %q", o.Container)
				}
				return io.NopCloser(strings.NewReader("")), nil
			}
			stream, err := worker.open(context.Background(), corev1.PodLogOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_ = stream.Close()
		})
	}
}

func TestContinuityCompleteRetryCursorDoesNotRepeatAttemptHeader(t *testing.T) {
	var output bytes.Buffer
	worker := testLogWorker(&output)
	worker.step.Attempt = "2"
	worker.complete = true
	worker.done = make(chan struct{})
	worker.markTerminal()
	worker.owner.reader = func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
		t.Error("complete retry cursor fetched a Pod")
		return io.NopCloser(strings.NewReader("")), nil
	}
	worker.run()
	if output.Len() != 0 {
		t.Fatal("complete retry cursor appended duplicate header", output.String())
	}
}
