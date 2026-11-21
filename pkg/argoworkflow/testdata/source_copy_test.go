package controller

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/util/logging"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestOberthSourceCopiesArePodPrivate(t *testing.T) {
	for _, kind := range []string{"container", "script", "container-set"} {
		t.Run(kind, func(t *testing.T) { checkSourceCopyPods(t, kind) })
	}
}

func checkSourceCopyPods(t *testing.T, kind string) {
	t.Helper()
	var wf wfv1.Workflow
	readCase(t, filepath.Join(os.Getenv("OBERTH_CONTROLLER_CASES"), "source-copy-"+kind+".json"), &wf)
	wf.Name, wf.Namespace = "source-copies", "default"
	wf.Spec.Executor = &wfv1.ExecutorConfig{ServiceAccountName: "default"}
	if patch := os.Getenv("OBERTH_CONTROLLER_POD_PATCH"); patch != "" {
		wf.Spec.PodSpecPatch = patch
	}
	ctx := logging.TestContext(t.Context())
	cancel, controller := newController(ctx, &wf, defaultServiceAccount, func(c *WorkflowController) { configureControllerSource(c) })
	defer cancel()
	woc := newWorkflowOperationCtx(ctx, &wf, controller)
	woc.operate(ctx)
	pods, err := controller.kubeclientset.CoreV1().Pods(wf.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(pods.Items) != 2 {
		t.Fatal("expected separate Pods for both source-consuming steps", len(pods.Items), woc.wf.Status, err)
	}
	if pods.Items[0].Name == pods.Items[1].Name {
		t.Fatal("steps share a Pod")
	}
	for _, pod := range pods.Items {
		count := 0
		for _, container := range pod.Spec.Containers {
			if container.Name != common.MainContainerName {
				continue
			}
			checkSourceCopyMount(t, pod, container, "/work/src")
			checkSourceCopyMount(t, pod, container, "/work/sources/library")
			count++
		}
		if count != 1 {
			t.Fatal("missing source-consuming main container", count)
		}
		if kind == "container-set" && len(pod.Spec.Containers) != 3 {
			t.Fatal("container-set must exercise both authored containers and wait")
		}
	}
}

func checkSourceCopyMount(t *testing.T, pod corev1.Pod, container corev1.Container, path string) {
	t.Helper()
	for _, mount := range container.VolumeMounts {
		if mount.MountPath != path {
			continue
		}
		for _, volume := range pod.Spec.Volumes {
			if volume.Name != mount.Name {
				continue
			}
			if volume.EmptyDir == nil || !reflect.DeepEqual(volume.VolumeSource, corev1.VolumeSource{EmptyDir: volume.EmptyDir}) {
				t.Fatal("source volume must belong to this Pod alone", pod.Name, container.Name, path, volume)
			}
			return
		}
		t.Fatal("source mount has no volume", pod.Name, container.Name, path)
	}
	t.Fatal("source mount is missing", pod.Name, container.Name, path)
}
