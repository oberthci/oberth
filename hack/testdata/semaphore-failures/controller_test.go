// Run in a disposable copy of pinned Argo's controller package. These errors
// come from its real synchronization limit reader and workflow operation path.
package controller

import (
	"encoding/json"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOberthSemaphoreConfigurationFailures(t *testing.T) {
	for _, missing := range []string{"configmap", "key"} {
		t.Run(missing, func(t *testing.T) {
			ctx := logging.TestContext(t.Context())
			wf := wfv1.MustUnmarshalWorkflow(helloWorldWf)
			wf.Namespace = "pipelines"
			wf.Spec.Synchronization = &wfv1.Synchronization{Semaphores: []*wfv1.SemaphoreRef{{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "oberth-sync-limits"}, Key: "build"}}}}
			woc := newWoc(ctx, *wf)
			if missing == "key" {
				_, err := woc.controller.kubeclientset.CoreV1().ConfigMaps(wf.Namespace).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "oberth-sync-limits", Namespace: wf.Namespace}, Data: map[string]string{"other": "1"}}, metav1.CreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			}
			woc.operate(ctx)
			if woc.wf.Status.Phase != wfv1.WorkflowFailed || !strings.Contains(woc.wf.Status.Message, "/ConfigMap/oberth-sync-limits/build") {
				t.Fatalf("wrong failure: %s: %s", woc.wf.Status.Phase, woc.wf.Status.Message)
			}
			body, err := json.Marshal(map[string]string{"missing": missing, "message": woc.wf.Status.Message})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("SEMAPHORE_FAILURE_PROOF=%s", body)
		})
	}
}
