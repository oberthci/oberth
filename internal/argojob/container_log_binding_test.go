package argojob

import (
	"context"
	"io"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/internal/runlog"
)

func TestNativeContainerSetLateChildRebindsOnePhysicalCopy(t *testing.T) {
	for _, mode := range []string{"complete-first-snapshot", "late-child", "already-captured"} {
		t.Run(mode, func(t *testing.T) {
			controller, store, workflow, pod := nativeContainerLogFixture(t)
			controller.config.MaxRunLogBytes = 1 << 20
			workflow.Status.Phase = wfv1.WorkflowRunning
			parent := workflow.Status.Nodes["node"]
			parent.Phase = wfv1.NodePending
			workflow.Status.Nodes[parent.ID] = parent
			pod.Spec.Containers = []corev1.Container{{Name: "build"}}
			pod.Status = corev1.PodStatus{Phase: corev1.PodRunning}
			child := wfv1.NodeStatus{ID: "build-child", Name: "workflow.task.build", DisplayName: "build", BoundaryID: parent.ID, Type: wfv1.NodeTypeContainer, Phase: wfv1.NodeRunning}
			if mode == "complete-first-snapshot" {
				workflow.Status.Nodes[child.ID] = child
			}
			if mode == "already-captured" {
				pod.Status.Phase = corev1.PodSucceeded
			}
			calls := 0
			controller.WithContainerLogSource(func(_ context.Context, _, _ string, options corev1.PodLogOptions) (io.ReadCloser, error) {
				calls++
				if options.Container != "build" {
					t.Fatal("wrong physical container", options)
				}
				return io.NopCloser(strings.NewReader("build output\n")), nil
			}, func(context.Context, string, string) (*corev1.Pod, error) { return pod.DeepCopy(), nil })
			if err := controller.ObserveContainerLogs(t.Context(), workflow, "run", []*corev1.Pod{pod}); err != nil {
				t.Fatal(err)
			}
			before, err := store.ContainerLogs(t.Context(), "run")
			if err != nil || len(before) != 1 {
				t.Fatal(before, err)
			}
			workflow.Status.Phase = wfv1.WorkflowFailed
			parent.Phase, child.Phase = wfv1.NodeFailed, wfv1.NodeFailed
			workflow.Status.Nodes[parent.ID], workflow.Status.Nodes[child.ID] = parent, child
			pod.Status.Phase = corev1.PodFailed
			for range 2 {
				if err := controller.ObserveContainerLogs(t.Context(), workflow, "run", []*corev1.Pod{pod}); err != nil {
					t.Fatal(err)
				}
			}
			body, _, saved, err := store.ReadContainerLog(t.Context(), "run", child.ID, "build", runlog.Filter{})
			if err != nil || string(body) != "build output\n" || calls != 1 || !saved.Main || saved.Step != "build" || saved.Stage != "workflow.task" {
				t.Fatal("late child lost copy or display attribution", string(body), saved, calls, err)
			}
			if mode == "already-captured" && (saved.Start != before[0].Start || saved.End != before[0].End || !saved.Captured) {
				t.Fatal("rebinding changed the stored span", before, saved)
			}
		})
	}
}
