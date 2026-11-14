package argojob

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/internal/runlog"
)

func TestStoppedCompletedWorkflowResumesLogsAndRetiresExecutorIdentity(t *testing.T) {
	workflow, request := adoptionWorkflow()
	workflow.Spec.Shutdown = wfv1.ShutdownStrategyStop
	workflow.Status.Phase = wfv1.WorkflowFailed
	store, err := runlog.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	controller := &Controller{kube: fixtureKube(workflow), workflows: &staticWorkflowClient{workflow: workflow}, config: Config{Namespace: workflow.Namespace, ExecutorServiceAccount: "executor", MaxRunLogBytes: 4096}, idleInterval: time.Millisecond, containerLogs: store}
	var pods []*corev1.Pod
	for _, pod := range fixtureStepPods(workflow) {
		pod.Spec.Containers = []corev1.Container{{Name: "main"}}
		pod.Status.Phase = corev1.PodSucceeded
		pods = append(pods, pod)
	}
	controller.hub = containerLogCache{workflow: workflow, pods: pods}
	controller.WithContainerLogSource(func(context.Context, string, string, corev1.PodLogOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("final exit log\n")), nil
	}, func(_ context.Context, _, name string) (*corev1.Pod, error) {
		for _, pod := range pods {
			if pod.Name == name {
				return pod.DeepCopy(), nil
			}
		}
		return nil, apierrors.NewNotFound(corev1.Resource("pods"), name)
	})
	holder := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: controller.identityPodName(workflow, "executor"), Namespace: workflow.Namespace, UID: "identity-uid", Labels: map[string]string{"oberth.ci/role": "executor-identity"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: workflow.Name, UID: workflow.UID, Controller: ptr.To(true)}}}, Spec: holderSpec("")}
	if _, err := controller.kube.CoreV1().Pods(workflow.Namespace).Create(t.Context(), holder, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	binding, found, err := controller.Adopt(t.Context(), request)
	if err != nil || !found {
		t.Fatal("completed Workflow not adopted", found, err)
	}
	completion, err := controller.WaitResumed(t.Context(), workflow.Name, request.RunID, io.Discard, nil, 0, binding)
	if err != nil || !completion.Terminal {
		t.Fatalf("final observation: %+v %v", completion, err)
	}
	for _, pod := range pods {
		body, _, _, err := store.ReadContainerLog(t.Context(), request.RunID, pod.Annotations["workflows.argoproj.io/node-id"], "main", runlog.Filter{})
		if err != nil || string(body) != "final exit log\n" {
			t.Fatalf("final logs missing: %q %v", body, err)
		}
	}
	if _, err := controller.kube.CoreV1().Pods(workflow.Namespace).Get(t.Context(), holder.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("completed Workflow retained executor identity: %v", err)
	}
}
