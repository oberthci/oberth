package argojob

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/runlog"
)

type notifyingContainerLogStore struct {
	*runlog.Store
	tracked chan struct{}
}

func (store notifyingContainerLogStore) TrackContainerLogs(runID string, entries []runlog.ContainerLog) error {
	if err := store.Store.TrackContainerLogs(runID, entries); err != nil {
		return err
	}
	select {
	case store.tracked <- struct{}{}:
	default:
	}
	return nil
}

func TestContainerLogShutdownResumesUnstartedCopies(t *testing.T) {
	for _, mode := range []string{"uninterrupted", "shutdown", "shutdown-during-copy"} {
		t.Run(mode, func(t *testing.T) {
			controller, store, workflow, pod := nativeContainerLogFixture(t)
			controller.config.MaxRunLogBytes = 1 << 20
			workflow.Status.Phase = wfv1.WorkflowRunning
			node := workflow.Status.Nodes["node"]
			node.Phase = wfv1.NodeRunning
			workflow.Status.Nodes[node.ID] = node
			pod.Spec.Containers = []corev1.Container{{Name: "main"}, {Name: "later"}}
			pod.Status = corev1.PodStatus{Phase: corev1.PodRunning}
			if mode == "shutdown-during-copy" {
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
			}
			hub := newControlledWaitHub(workflow.DeepCopy())
			hub.setPods(pod.DeepCopy())
			controller.hub = hub
			tracked := make(chan struct{}, 1)
			controller.containerLogs = notifyingContainerLogStore{Store: store, tracked: tracked}
			calls := map[string]int{}
			opened := make(chan struct{}, 1)
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			open := func(_ context.Context, _, _ string, options corev1.PodLogOptions) (io.ReadCloser, error) {
				calls[options.Container]++
				if mode == "shutdown-during-copy" && options.Container == "main" && calls["main"] == 1 {
					opened <- struct{}{}
					return reader, nil
				}
				return io.NopCloser(strings.NewReader(options.Container + " output\n")), nil
			}
			// The observed Pod identity is immutable throughout the first wait.
			observedPod := pod.DeepCopy()
			getPod := func(context.Context, string, string) (*corev1.Pod, error) { return observedPod.DeepCopy(), nil }
			controller.WithContainerLogSource(open, getPod)
			settled := workflow.DeepCopy()
			settled.Status.Phase = wfv1.WorkflowSucceeded
			finalNode := node
			finalNode.Phase = wfv1.NodeSucceeded
			settled.Status.Nodes[finalNode.ID] = finalNode
			controller.workflows = &staticWorkflowClient{workflow: settled}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := controller.Wait(ctx, workflow.Name, "run", io.Discard); done <- err }()
			select {
			case <-tracked:
			case <-time.After(3 * time.Second):
				t.Fatal("wait did not track running containers")
			}
			if mode == "shutdown-during-copy" {
				select {
				case <-opened:
				case <-time.After(3 * time.Second):
					t.Fatal("copy did not start")
				}
				if _, err := writer.Write([]byte("partial output\n")); err != nil {
					t.Fatal(err)
				}
			}
			workflow.Status.Phase = wfv1.WorkflowSucceeded
			node.Phase = wfv1.NodeSucceeded
			workflow.Status.Nodes[node.ID] = node
			pod.Status = corev1.PodStatus{Phase: corev1.PodSucceeded}
			if mode == "uninterrupted" {
				hub.setPods(pod.DeepCopy())
				hub.setWorkflow(workflow)
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if mode == "uninterrupted" && err != nil {
					t.Fatal(err)
				}
				if mode != "uninterrupted" && !errors.Is(err, context.Canceled) {
					t.Fatal("shutdown lost cancellation", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("wait did not join copy task")
			}
			if mode != "uninterrupted" {
				entries, err := store.ContainerLogs(t.Context(), "run")
				if err != nil || len(entries) != 2 {
					t.Fatal(entries, err)
				}
				for _, entry := range entries {
					if mode == "shutdown-during-copy" && entry.ContainerName == "main" {
						if !entry.Captured || entry.Reason == "" {
							t.Fatal("attempted copy lost its receipt", entry)
						}
					} else if entry.Captured || entry.Pending || entry.Reason != "" {
						t.Fatal("shutdown sealed an unstarted copy", entry)
					}
				}
				// A fresh observer adopts the same Workflow and physical containers.
				resumed := &Controller{config: controller.config, containerLogs: store, workflows: &staticWorkflowClient{workflow: workflow}, hub: containerLogCache{workflow: workflow, pods: []*corev1.Pod{pod}}}
				resumed.WithContainerLogSource(open, func(context.Context, string, string) (*corev1.Pod, error) { return pod.DeepCopy(), nil })
				digest, err := adoptionSpecHash(workflow)
				if err != nil {
					t.Fatal(err)
				}
				binding := Adoption{UID: workflow.UID, Identity: workflow.Annotations[identityAnnotation], SpecSHA256: digest}
				if _, err := resumed.WaitResumed(t.Context(), workflow.Name, "run", io.Discard, binding); err != nil {
					t.Fatal(err)
				}
			}
			for _, container := range []string{"main", "later"} {
				body, _, saved, err := store.ReadContainerLog(t.Context(), "run", "node", container, runlog.Filter{})
				want := container + " output\n"
				if mode == "shutdown-during-copy" && container == "main" {
					want = "partial output\n"
				}
				if err != nil || !saved.Captured || !strings.HasPrefix(string(body), want) || calls[container] != 1 {
					t.Fatal("resume lost or repeated a physical copy", container, string(body), saved, calls, err)
				}
			}
		})
	}
}

func TestContainerLogCancelledFinalPassLeavesUnstartedCopiesEligible(t *testing.T) {
	controller, store, workflow, pod := nativeContainerLogFixture(t)
	controller.config.MaxRunLogBytes = 1 << 20
	pod.Spec.Containers = []corev1.Container{{Name: "first"}, {Name: "second"}, {Name: "third"}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	controller.WithContainerLogSource(func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
		calls++
		if calls == 1 {
			cancel()
			return nil, ctx.Err()
		}
		return io.NopCloser(strings.NewReader("remaining output\n")), nil
	}, func(context.Context, string, string) (*corev1.Pod, error) { return pod.DeepCopy(), nil })
	err := controller.ObserveContainerLogs(ctx, workflow, "run", []*corev1.Pod{pod})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("final snapshot ignored observer cancellation", err)
	}
	entries, err := store.ContainerLogs(t.Context(), "run")
	if err != nil || len(entries) != 3 || calls != 1 || !entries[0].Captured {
		t.Fatal(entries, calls, err)
	}
	for _, entry := range entries[1:] {
		if entry.Captured || entry.Pending || entry.Reason != "" {
			t.Fatal("cancelled final pass sealed an unstarted copy", entry)
		}
	}
	for range 2 {
		if err := controller.ObserveContainerLogs(t.Context(), workflow, "run", []*corev1.Pod{pod}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Fatal("adoption lost or repeated a physical copy", calls)
	}
	for _, entry := range entries[1:] {
		body, _, saved, err := store.ReadContainerLog(t.Context(), "run", "node", entry.ContainerName, runlog.Filter{})
		if err != nil || !saved.Captured || string(body) != "remaining output\n" {
			t.Fatal(string(body), saved, err)
		}
	}
}
