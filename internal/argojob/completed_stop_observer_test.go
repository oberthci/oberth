package argojob

import (
	"bytes"
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
)

func TestStoppedCompletedWorkflowResumesLogsAndRetiresExecutorIdentity(t *testing.T) {
	workflow, request := adoptionWorkflow()
	workflow.Spec.Shutdown = wfv1.ShutdownStrategyStop
	workflow.Status.Phase = wfv1.WorkflowFailed
	controller := &Controller{kube: fixtureKube(workflow), workflows: &staticWorkflowClient{workflow: workflow}, config: Config{Namespace: workflow.Namespace, ExecutorServiceAccount: "executor", MaxRunLogBytes: 4096}, interval: time.Millisecond,
		logs: func(context.Context, string, string, string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("final exit log\n")), nil
		},
	}
	holder := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: controller.identityPodName(workflow, "executor"), Namespace: workflow.Namespace, UID: "identity-uid", Labels: map[string]string{"oberth.ci/role": "executor-identity"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "argoproj.io/v1alpha1", Kind: "Workflow", Name: workflow.Name, UID: workflow.UID, Controller: ptr.To(true)}}}}
	if _, err := controller.kube.CoreV1().Pods(workflow.Namespace).Create(t.Context(), holder, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	binding, found, err := controller.Adopt(t.Context(), request)
	if err != nil || !found {
		t.Fatal("completed Workflow not adopted", found, err)
	}
	var output bytes.Buffer
	completion, err := controller.WaitResumed(t.Context(), workflow.Name, request.RunID, &output, nil, 0, binding)
	if err != nil || !completion.Terminal {
		t.Fatalf("final observation: %+v %v", completion, err)
	}
	if !strings.Contains(output.String(), "final exit log") {
		t.Fatalf("final logs missing: %q", output.String())
	}
	if _, err := controller.kube.CoreV1().Pods(workflow.Namespace).Get(t.Context(), holder.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("completed Workflow retained executor identity: %v", err)
	}
}
