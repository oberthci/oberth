package argojob

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Model Argo's executor.env followed by the real strategic PodSpecPatch merge.
// In particular, merging by name does not remove a second controller entry.
func executorEnvironmentPod(t *testing.T, shape, target string, env []corev1.EnvVar) corev1.PodSpec {
	t.Helper()
	controller, _, _, _ := executorRetryFixture(t)
	leaf := wfv1.Template{Name: "step", Container: &corev1.Container{Image: "busybox"}}
	wf := &wfv1.Workflow{}
	switch shape {
	case "direct", "podSpecPatch-free":
	case "templateDefaults":
		wf.Spec.TemplateDefaults = &wfv1.Template{Container: &corev1.Container{Env: []corev1.EnvVar{{Name: "KUBECONFIG", Value: "author-default"}}}}
	case "containerSet":
		leaf.Container = nil
		leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: corev1.Container{Name: "main", Image: "busybox"}}}}
	case "script":
		leaf.Container = nil
		leaf.Script = &wfv1.ScriptTemplate{Container: corev1.Container{Image: "busybox"}, Source: "true"}
	case "resource":
		leaf.Container = nil
		leaf.Resource = &wfv1.ResourceTemplate{Action: "get", Manifest: "{}"}
	case "data":
		leaf.Container = nil
		leaf.Data = &wfv1.Data{}
	default:
		t.Fatalf("unknown shape %s", shape)
	}
	wf.Spec.Templates = []wfv1.Template{leaf}
	if leaf.PodSpecPatch != "" {
		t.Fatal("fixture must not rely on authored podSpecPatch")
	}
	if err := projectExecutorTokens(wf, controller.config); err != nil {
		t.Fatal(err)
	}
	mounts := []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir}}
	before := corev1.PodSpec{InitContainers: []corev1.Container{{Name: "init", VolumeMounts: mounts}}}
	name := "wait"
	if shape == "resource" || shape == "data" {
		name = "main"
	}
	before.Containers = []corev1.Container{{Name: name, Image: argoworkflow.ExecutorImage, VolumeMounts: mounts}}
	if name == "wait" {
		main := corev1.Container{Name: "main", Image: "busybox"}
		if wf.Spec.TemplateDefaults != nil {
			main.Env = wf.Spec.TemplateDefaults.Container.Env
		}
		before.Containers = append(before.Containers, main)
	}
	for i := range before.InitContainers {
		if before.InitContainers[i].Name == target {
			before.InitContainers[i].Env = env
		}
	}
	for i := range before.Containers {
		if before.Containers[i].Name == target {
			before.Containers[i].Env = env
		}
	}
	return applyCredentialWaitPatch(t, before, wf.Spec.Templates[0].PodSpecPatch)
}

func TestExecutorIdentityRejectsDuplicateEnvironment(t *testing.T) {
	for _, shape := range []string{"direct", "templateDefaults", "containerSet", "podSpecPatch-free", "script", "resource", "data"} {
		for _, target := range []string{"init", "wait", "main"} {
			if (target == "wait" && (shape == "resource" || shape == "data")) || (target == "main" && shape != "resource" && shape != "data") {
				continue
			}
			for _, expected := range executorEnvironment {
				for _, duplicate := range []string{"unsafe", "identical"} {
					t.Run(shape+"/"+target+"/"+expected[0]+"/"+duplicate, func(t *testing.T) {
						value := "author-wins"
						if duplicate == "identical" {
							value = expected[1]
						}
						env := []corev1.EnvVar{{Name: expected[0], Value: value}, {Name: expected[0], Value: value}}
						controller, wf, pod, requests := executorRetryFixture(t)
						pod.Spec = executorEnvironmentPod(t, shape, target, env)
						// The live Pod must retain both entries after Argo's merge.
						count := 0
						for _, c := range append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...) {
							if c.Name == target {
								for _, e := range c.Env {
									if e.Name == expected[0] {
										count++
									}
								}
							}
						}
						if count != 2 {
							t.Fatalf("strategic merge removed duplicate: count=%d", count)
						}
						if shape == "resource" || shape == "data" {
							// Exercise the operator/data executor main allowance directly.
							if err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, true); err == nil {
								t.Fatal("executor main accepted duplicate environment")
							}
							return
						}
						if _, err := controller.kube.CoreV1().Pods(wf.Namespace).Update(t.Context(), pod, metav1.UpdateOptions{}); err != nil {
							t.Fatal(err)
						}
						attempts := 0
						controller.seeder.exec = func(context.Context, string, string, string, []string, io.Reader, io.Writer, io.Writer) error {
							attempts++
							return nil
						}
						delivered := map[types.UID]bool{}
						err := controller.deliverExecutorTokens(t.Context(), wf, delivered)
						var failure *executorIdentityError
						if !errors.As(err, &failure) || failure.stage != "target_validation" || *requests != 0 || attempts != 0 || delivered[pod.UID] {
							t.Fatalf("unsafe executor reached delivery: err=%v requests=%d exec=%d delivered=%v", err, *requests, attempts, delivered)
						}
					})
				}
			}
		}
	}
}

func TestExecutorIdentityRejectsEnvironmentDrift(t *testing.T) {
	for _, expected := range executorEnvironment {
		for _, drift := range []string{"missing", "wrong", "valueFrom"} {
			t.Run(expected[0]+"/"+drift, func(t *testing.T) {
				controller, _, pod, _ := executorRetryFixture(t)
				pod.Spec = executorEnvironmentPod(t, "direct", "wait", nil)
				for i := range pod.Spec.Containers {
					c := &pod.Spec.Containers[i]
					if c.Name != "wait" {
						continue
					}
					for j := range c.Env {
						if c.Env[j].Name != expected[0] {
							continue
						}
						switch drift {
						case "missing":
							c.Env = append(c.Env[:j], c.Env[j+1:]...)
						case "wrong":
							c.Env[j].Value = "unsafe"
						case "valueFrom":
							c.Env[j].ValueFrom = &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}
						}
						break
					}
				}
				if err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, false); err == nil || !strings.Contains(err.Error(), expected[0]) {
					t.Fatalf("executor environment drift accepted: %v", err)
				}
			})
		}
	}
}

func TestExecutorIdentityAcceptsExactEnvironment(t *testing.T) {
	for _, shape := range []string{"direct", "templateDefaults", "containerSet", "podSpecPatch-free", "script", "resource", "data"} {
		t.Run(shape, func(t *testing.T) {
			controller, _, pod, _ := executorRetryFixture(t)
			// A single controller default is overwritten by the server patch.
			pod.Spec = executorEnvironmentPod(t, shape, "init", []corev1.EnvVar{{Name: "KUBECONFIG", Value: "controller-default"}, {Name: "UNRELATED", Value: "kept"}})
			if err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, shape == "resource" || shape == "data"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
