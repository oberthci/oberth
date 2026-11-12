// Run in a disposable copy of the pinned Argo module's workflow/controller
// package. Pass the exact posted Workflow path after -args, captured by
// TestNativeAuthorLabelsAndAntiAffinityReachWorkflowAPI. This exercises the
// real pinned controller's Pod builder, not a duplicate metadata merger.
package controller

import (
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOberthPostedAuthorMetadataReachesPod(t *testing.T) {
	if len(flag.Args()) != 1 {
		t.Fatal("pass the posted Workflow path after -args")
	}
	raw, err := os.ReadFile(flag.Args()[0])
	if err != nil {
		t.Fatal(err)
	}
	var wf wfv1.Workflow
	if err = json.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	ctx := logging.TestContext(t.Context())
	woc := newWoc(ctx, wf)
	tmpl := &wf.Spec.Templates[0]
	if _, err = woc.controller.kubeclientset.CoreV1().ServiceAccounts(wf.Namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "oberth-argo-executor", Namespace: wf.Namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod, err := woc.createWorkflowPod(ctx, wf.Name, []corev1.Container{*tmpl.Container}, tmpl, &createWorkflowPodOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"app": "store", "security": "S2"} {
		if pod.Labels[key] != want {
			t.Fatalf("Pod %s=%q, want %q", key, pod.Labels[key], want)
		}
	}
	for _, key := range []string{"oberth.ci/run", "oberth.ci/trigger", "oberth.ci/tier"} {
		if pod.Labels[key] != wf.Spec.PodMetadata.Labels[key] || pod.Labels[key] == "" {
			t.Fatal("server label lost", key, pod.Labels)
		}
	}
	if !reflect.DeepEqual(pod.Spec.Affinity, wf.Spec.Affinity) {
		t.Fatal("authored anti-affinity changed", pod.Spec.Affinity)
	}
	encoded, _ := json.Marshal(pod.ObjectMeta)
	t.Logf("actual Pod metadata: %s", encoded)
}
