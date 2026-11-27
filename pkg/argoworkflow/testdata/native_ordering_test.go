package controller

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOberthNativePriorityAndSerialPods(t *testing.T) {
	t.Setenv("INFORMER_WRITE_BACK", "true")
	for _, override := range []string{"", "oberth-run-lint-oberth"} {
		for _, parallelism := range []int64{1, 2} {
			ctx := logging.TestContext(t.Context())
			priority := int32(30)
			w := &wfv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "ordering", Namespace: "default"}, Spec: wfv1.WorkflowSpec{
				Entrypoint: "main", Priority: &priority, PodPriorityClassName: "oberth-run-release-oberth", Parallelism: &parallelism,
				Templates: []wfv1.Template{
					{Name: "main", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "a", Template: "leaf"}, {Name: "b", Template: "leaf"}, {Name: "c", Template: "leaf"}}}},
					{Name: "leaf", PriorityClassName: override, Container: &corev1.Container{Image: "image", Command: []string{"true"}}},
				},
			}}
			cancel, controller := newController(ctx, w, defaultServiceAccount)
			woc := newWorkflowOperationCtx(ctx, w, controller)
			woc.operate(ctx)
			pods, err := controller.kubeclientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{})
			if err != nil || int64(len(pods.Items)) != parallelism {
				cancel()
				t.Fatal("native controller changed intentional fanout", parallelism, len(pods.Items), err)
			}
			want := w.Spec.PodPriorityClassName
			if override != "" {
				want = override
			}
			for _, pod := range pods.Items {
				if pod.Spec.PriorityClassName != want {
					cancel()
					t.Fatal("native Pod priority override/default lost", pod.Spec.PriorityClassName, want)
				}
			}
			makePodsPhase(ctx, woc, corev1.PodRunning)
			woc.operate(ctx)
			pods, err = controller.kubeclientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{})
			cancel()
			if err != nil || int64(len(pods.Items)) != parallelism {
				t.Fatal("running serial work gained extra Pods", err)
			}
		}
	}
}
