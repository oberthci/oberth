package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	argofile "github.com/argoproj/argo-workflows/v4/util/file"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/runlog"
)

const nativeLogMarkerFixture = "@@oberth-step-progress@@ " + `{"version":1,"burn":"test","step":"one","ordinal":0,"status":"queued"}` + "\n" + "body\n"

type containerLogCache struct {
	workflow *wfv1.Workflow
	pods     []*corev1.Pod
}

func (cache containerLogCache) Workflow(name string) (*wfv1.Workflow, bool) {
	return cache.workflow, cache.workflow != nil && cache.workflow.Name == name
}
func (cache containerLogCache) PodsOf(string) []*corev1.Pod { return cache.pods }
func (cache containerLogCache) Pods() []*corev1.Pod         { return cache.pods }
func (containerLogCache) Healthy() bool                     { return true }
func (containerLogCache) Synced() bool                      { return true }
func (containerLogCache) UnhealthySince() (time.Time, bool) { return time.Time{}, false }

func nativeContainerLogFixture(t *testing.T) (*Controller, *runlog.Store, *wfv1.Workflow, *corev1.Pod) {
	t.Helper()
	workflow := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "workflow", Namespace: "pipeline", UID: "workflow-uid", Annotations: map[string]string{runIDAnnotation: "run"}}, Status: wfv1.WorkflowStatus{Phase: wfv1.WorkflowSucceeded, Nodes: wfv1.Nodes{"node": {ID: "node", Name: "workflow.task", Type: wfv1.NodeTypePod, Phase: wfv1.NodeSucceeded}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "opaque-pod-name", Namespace: "pipeline", UID: "pod-uid", Annotations: map[string]string{"workflows.argoproj.io/node-id": "node", "workflows.argoproj.io/node-name": "workflow.task"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: workflow.Name, UID: workflow.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded, ContainerStatuses: []corev1.ContainerStatus{{Name: "main", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}}}
	store, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	controller := &Controller{config: Config{Namespace: "pipeline", MaxRunLogBytes: 128}, containerLogs: store, hub: containerLogCache{workflow: workflow, pods: []*corev1.Pod{pod}}}
	return controller, store, workflow, pod
}

type cutContainerLog struct{ body string }

func (reader *cutContainerLog) Read(out []byte) (int, error) {
	if reader.body == "" {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(out, reader.body)
	reader.body = reader.body[n:]
	return n, io.ErrUnexpectedEOF
}
func (*cutContainerLog) Close() error { return nil }

func TestContainerLogCaptureReadsOnceAndPreservesPartialBytes(t *testing.T) {
	for _, mode := range []string{"complete", "cut", "limit", "max-limit", "missing", "foreign", "swapped-after-open"} {
		t.Run(mode, func(t *testing.T) {
			controller, store, workflow, pod := nativeContainerLogFixture(t)
			if mode == "max-limit" {
				controller.config.MaxRunLogBytes = math.MaxInt64
			}
			calls, gets := 0, 0
			controller.WithContainerLogSource(func(_ context.Context, namespace, name string, options corev1.PodLogOptions) (io.ReadCloser, error) {
				calls++
				if namespace != pod.Namespace || name != pod.Name || options.Container != "main" || options.Follow || options.SinceTime != nil || options.Timestamps {
					t.Fatal("not a plain native copy", namespace, name, options)
				}
				if mode == "cut" {
					return &cutContainerLog{body: "retained partial\n"}, nil
				}
				if mode == "limit" {
					return io.NopCloser(strings.NewReader(strings.Repeat("x", 256))), nil
				}
				return io.NopCloser(strings.NewReader(nativeLogMarkerFixture)), nil
			}, func(context.Context, string, string) (*corev1.Pod, error) {
				gets++
				if mode == "missing" {
					return nil, apierrors.NewNotFound(corev1.Resource("pods"), pod.Name)
				}
				current := pod.DeepCopy()
				if mode == "foreign" || (mode == "swapped-after-open" && gets > 1) {
					current.UID = types.UID("different-pod")
				}
				return current, nil
			})
			for range 2 {
				if err := controller.ObserveContainerLogs(t.Context(), workflow, "run", []*corev1.Pod{pod}); err != nil {
					t.Fatal(err)
				}
			}
			wantCalls := 1
			if mode == "missing" || mode == "foreign" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatal("container copy repeated", calls, wantCalls)
			}
			body, _, entry, err := store.ReadContainerLog(t.Context(), "run", "node", "main", runlog.Filter{})
			if err != nil || !entry.Captured {
				t.Fatal(string(body), entry, err)
			}
			if (mode == "complete" || mode == "max-limit") && string(body) != nativeLogMarkerFixture {
				t.Fatal("native bytes rewritten", string(body))
			}
			if mode == "cut" && (!strings.Contains(string(body), "retained partial") || !strings.Contains(string(body), "log incomplete")) {
				t.Fatal(string(body))
			}
			if mode == "limit" && (len(body) > 128 || !strings.Contains(string(body), "limit reached")) {
				t.Fatal(string(body))
			}
			if (mode == "missing" || mode == "foreign" || mode == "swapped-after-open") && !strings.Contains(string(body), "log unavailable") {
				t.Fatal(string(body))
			}
		})
	}
}

func TestContainerLogPodDeletedBeforeObservationHasReason(t *testing.T) {
	controller, store, workflow, _ := nativeContainerLogFixture(t)
	if err := controller.ObserveContainerLogs(t.Context(), workflow, "run", nil); err != nil {
		t.Fatal(err)
	}
	body, _, _, err := store.ReadContainerLog(t.Context(), "run", "node", "", runlog.Filter{})
	if err != nil || !strings.Contains(string(body), "Pod containers were not observed") {
		t.Fatal(string(body), err)
	}
}

type countedContainerStream struct {
	*io.PipeReader
	closes atomic.Int32
}

func (reader *countedContainerStream) Close() error {
	reader.closes.Add(1)
	return reader.PipeReader.Close()
}

func TestContainerLogLiveRequestCancellationClosesNativeStream(t *testing.T) {
	controller, _, workflow, pod := nativeContainerLogFixture(t)
	workflow.Status.Phase = wfv1.WorkflowRunning
	reader, writer := io.Pipe()
	defer writer.Close()
	stream := &countedContainerStream{PipeReader: reader}
	controller.WithContainerLogSource(func(_ context.Context, _, _ string, options corev1.PodLogOptions) (io.ReadCloser, error) {
		if !options.Follow || options.SinceTime != nil || options.Timestamps {
			t.Fatal("live request changed native options", options)
		}
		return stream, nil
	}, func(context.Context, string, string) (*corev1.Pod, error) { return pod.DeepCopy(), nil })
	ctx, cancel := context.WithCancel(t.Context())
	opened, err := controller.OpenContainerLog(ctx, workflow.Name, "run", "node", "main", true)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, opened); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("request cancellation leaked its read")
	}
	_ = opened.Close()
	if stream.closes.Load() != 1 {
		t.Fatal("native stream closed more than once", stream.closes.Load())
	}
}

func TestContainerLogCompressedNativeStatusDoesNotMutateCache(t *testing.T) {
	controller, store, workflow, pod := nativeContainerLogFixture(t)
	nodes, err := json.Marshal(workflow.Status.Nodes)
	if err != nil {
		t.Fatal(err)
	}
	workflow.Status.CompressedNodes = argofile.CompressEncodeString(t.Context(), string(nodes))
	workflow.Status.Nodes = nil
	compressed := workflow.Status.CompressedNodes
	calls := 0
	controller.WithContainerLogSource(func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
		calls++
		return io.NopCloser(strings.NewReader("compressed native output\n")), nil
	}, func(context.Context, string, string) (*corev1.Pod, error) { return pod.DeepCopy(), nil })
	if err := controller.ObserveContainerLogs(t.Context(), workflow, "run", []*corev1.Pod{pod}); err != nil {
		t.Fatal(err)
	}
	body, _, _, err := store.ReadContainerLog(t.Context(), "run", "node", "main", runlog.Filter{})
	if err != nil || string(body) != "compressed native output\n" || calls != 1 {
		t.Fatal(string(body), err, calls)
	}
	if workflow.Status.CompressedNodes != compressed || len(workflow.Status.Nodes) != 0 {
		t.Fatal("shared compressed status was changed")
	}
}

func (cache containerLogCache) Subscribe(string) (<-chan struct{}, func()) {
	return make(chan struct{}), func() {}
}
