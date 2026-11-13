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
	"slices"
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
	if err = woc.updateWorkflowMetadata(ctx); err != nil {
		t.Fatal(err)
	}
	for key, value := range wf.Spec.WorkflowMetadata.Annotations {
		if woc.wf.Annotations[key] != value {
			t.Fatal("workflowMetadata was not applied to Workflow", key)
		}
	}
	tmpl := &wf.Spec.Templates[0]
	if _, err = woc.controller.kubeclientset.CoreV1().ServiceAccounts(wf.Namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "oberth-argo-executor", Namespace: wf.Namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod, err := woc.createWorkflowPod(ctx, wf.Name, []corev1.Container{*tmpl.Container}, tmpl, &createWorkflowPodOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, annotations := range []map[string]string{wf.Annotations, wf.Spec.WorkflowMetadata.Annotations} {
		if len(annotations) == 0 {
			t.Fatal("missing Workflow-object annotation regression input")
		}
		for key := range annotations {
			if _, exists := pod.Annotations[key]; exists {
				t.Fatal("Workflow-object annotation propagated to Pod", key)
			}
		}
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
	if len(pod.Spec.NodeSelector) != 0 {
		t.Fatal("Oberth injected a Pod node selector", pod.Spec.NodeSelector)
	}
	var main *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "main" {
			main = &pod.Spec.Containers[i]
		}
	}
	if main == nil {
		t.Fatal("missing main")
	}
	// The pinned Argo emissary launcher is intrinsic to Argo execution. Compare
	// its child argv, while the posted Workflow itself retains authored argv.
	if len(main.Command) < 3 || main.Command[0] != "/var/run/argo/argoexec" || main.Command[1] != "emissary" {
		t.Fatal("unexpected Argo launcher", main.Command)
	}
	delimiter := slices.Index(main.Command, "--")
	if delimiter < 0 || !slices.Equal(main.Command[delimiter+1:], tmpl.Container.Command) || !slices.Equal(main.Args, tmpl.Container.Args) {
		t.Fatal("authored argv changed", main.Command, main.Args)
	}
	for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
		for _, mount := range container.VolumeMounts {
			if mount.MountPath == "/run/oberth/usage" {
				t.Fatal("measurement mount remains")
			}
		}
	}
	encoded, _ := json.Marshal(pod.ObjectMeta)
	t.Logf("actual Pod metadata: %s", encoded)
}
