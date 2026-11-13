package argojob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Model Argo's executor.env followed by the real strategic PodSpecPatch merge.
// In particular, merging by name does not remove a second controller entry.
func executorEnvironmentPod(t *testing.T, shape, target string, env []corev1.EnvVar) corev1.PodSpec {
	t.Helper()
	controller, _, _, _ := executorRetryFixture(t)
	leaf := wfv1.Template{Name: "step", Container: &corev1.Container{Image: "pipeline-image"}}
	wf := &wfv1.Workflow{}
	switch shape {
	case "container":
	case "resource", "resource-with-logs":
		leaf.Container = nil
		leaf.Resource = &wfv1.ResourceTemplate{Action: "get", Manifest: "{}"}
		if shape == "resource-with-logs" {
			leaf.ArchiveLocation = &wfv1.ArtifactLocation{ArchiveLogs: ptr.To(true)}
		}
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
	before := corev1.PodSpec{InitContainers: []corev1.Container{{Name: "init", Image: argoworkflow.ExecutorImage, VolumeMounts: mounts}}}
	names := []string{"wait"}
	switch shape {
	case "resource":
		names = []string{"main"}
	case "resource-with-logs":
		names = append(names, "main")
	}
	for _, name := range names {
		before.Containers = append(before.Containers, corev1.Container{Name: name, Image: argoworkflow.ExecutorImage, VolumeMounts: mounts})
	}
	if shape == "container" {
		before.Containers = append(before.Containers, corev1.Container{Name: "main", Image: "pipeline-image"})
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
	for _, shape := range []string{"container", "resource", "resource-with-logs"} {
		for _, target := range []string{"init", "wait", "main"} {
			if (target == "wait" && shape == "resource") || (target == "main" && shape == "container") {
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
						if shape != "container" {
							// Exercise the resource executor main allowance directly.
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
				pod.Spec = executorEnvironmentPod(t, "container", "wait", nil)
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
	for _, shape := range []string{"container", "resource", "resource-with-logs"} {
		t.Run(shape, func(t *testing.T) {
			controller, _, pod, _ := executorRetryFixture(t)
			// A single controller default is overwritten by the server patch.
			pod.Spec = executorEnvironmentPod(t, shape, "init", []corev1.EnvVar{{Name: "KUBECONFIG", Value: "controller-default"}, {Name: "UNRELATED", Value: "kept"}})
			if err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, shape != "container"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Explicit env takes precedence over envFrom. Controller envFrom is harmless
// only while all three literal protections are still present exactly once.
func TestExecutorIdentityEnvFromCannotReplaceExplicitEnvironment(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint("missing=", missing), func(t *testing.T) {
			controller, _, pod, _ := executorRetryFixture(t)
			pod.Spec = executorEnvironmentPod(t, "container", "wait", nil)
			for i := range pod.Spec.Containers {
				c := &pod.Spec.Containers[i]
				if c.Name == "wait" {
					c.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "executor-env"}}}}
					if missing {
						c.Env = nil
					}
				}
			}
			err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, false)
			if (err != nil) != missing {
				t.Fatalf("envFrom bypassed literal protections or safe explicit env was refused: %v", err)
			}
		})
	}
}

func TestExecutorIdentityRejectsAuthoredHelpers(t *testing.T) {
	for _, tc := range []struct {
		name, image string
		initializer bool
	}{
		{"wait", "pipeline-image", false},
		{"wait", "pipeline-image", true},
		{"init", "pipeline-image", true},
		{"init", "pipeline-image", false},
		{"wait", argoworkflow.ExecutorImage, true},
		{"init", argoworkflow.ExecutorImage, false},
		{"main", argoworkflow.ExecutorImage, true},
	} {
		t.Run(fmt.Sprintf("%s/initializer=%v/image=%s", tc.name, tc.initializer, tc.image), func(t *testing.T) {
			controller, _, pod, _ := executorRetryFixture(t)
			pod.Spec = executorEnvironmentPod(t, "resource", "main", nil)
			helper := corev1.Container{Name: tc.name, Image: tc.image, VolumeMounts: []corev1.VolumeMount{{Name: executorTokenVolume, MountPath: executorTokenDir}}}
			for _, expected := range executorEnvironment {
				helper.Env = append(helper.Env, corev1.EnvVar{Name: expected[0], Value: expected[1]})
			}
			if tc.initializer {
				pod.Spec.InitContainers = append(pod.Spec.InitContainers, helper)
			} else {
				pod.Spec.Containers = append(pod.Spec.Containers, helper)
			}
			if err := validateExecutorTokenTarget(pod, controller.config.SourceSeedImage, true); err == nil {
				t.Fatal("authored helper can read the operator executor token")
			}
		})
	}
}
