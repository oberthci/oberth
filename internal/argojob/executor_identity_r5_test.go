package argojob

import (
	"encoding/json"
	"fmt"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

func authoredInitMergedPod(t *testing.T, shape string, restartable bool) *corev1.Pod {
	t.Helper()
	authored, err := argoworkflow.Decode([]byte(greedyDocument))
	if err != nil {
		t.Fatal(err)
	}
	leaf := &authored.Spec.Templates[0]
	if shape == "script" {
		leaf.Script = &wfv1.ScriptTemplate{Container: *leaf.Container, Source: "true"}
		leaf.Container = nil
	}
	if shape == "containerSet" {
		leaf.Container.Name = "main"
		leaf.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: *leaf.Container}}}
		leaf.Container = nil
	}
	helper := corev1.Container{Name: "authored-init", Image: "golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Command: []string{"sh", "-c", "chmod u+w /var/run/argo/template; printf '%s' '{}' > /var/run/argo/template"}, VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: "/work"}}}
	if restartable {
		helper.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
	}
	leaf.InitContainers = []wfv1.UserContainer{{Container: helper}}
	leaf.Sidecars = []wfv1.UserContainer{{Container: corev1.Container{Name: "companion", Image: helper.Image, Command: []string{"true"}}}}
	authored.Spec.Volumes = append(authored.Spec.Volumes, corev1.Volume{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("1Mi"))}}})
	body, err := json.Marshal(authored)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := Build(testConfig(), testRequest(pipeline.TriggerCI, string(body)))
	if err != nil {
		t.Fatal(err)
	}
	// Rehydrate current Build's authored containers and controller environment
	// at workflowpod.go's actual pre-patch boundary. Argo adds this mount before
	// patching initContainers, and after patching ordinary containers/sidecars.
	before := executorEnvironmentPod(t, "container", "init", nil)
	var initializers []corev1.Container
	for _, c := range before.InitContainers {
		if c.Name != executorTokenWaiter {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "var-run-argo", MountPath: "/var/run/argo"})
			initializers = append(initializers, c)
		}
	}
	for _, c := range wf.Spec.Templates[0].InitContainers {
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "var-run-argo", MountPath: "/var/run/argo"})
		initializers = append(initializers, c.Container)
	}
	before.InitContainers = initializers
	for _, c := range wf.Spec.Templates[0].Sidecars {
		before.Containers = append(before.Containers, c.Container)
	}
	for _, patch := range []string{wf.Spec.PodSpecPatch, wf.Spec.Templates[0].PodSpecPatch} {
		if patch != "" {
			before = applyCredentialWaitPatch(t, before, patch)
		}
	}
	for i := range before.Containers {
		before.Containers[i].VolumeMounts = append(before.Containers[i].VolumeMounts, corev1.VolumeMount{Name: "var-run-argo", MountPath: "/var/run/argo"})
	}
	return &corev1.Pod{Spec: before}
}

func TestExecutorIdentityAuthoredInitReadOnlyMerge(t *testing.T) {
	for _, shape := range []string{"container", "script", "containerSet"} {
		for _, restartable := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restartable=%v", shape, restartable), func(t *testing.T) {
				pod := authoredInitMergedPod(t, shape, restartable)
				found := false
				for _, c := range pod.Spec.InitContainers {
					if c.Name != "authored-init" {
						continue
					}
					found = true
					if (c.RestartPolicy != nil) != restartable {
						t.Fatal("merge changed authored restart policy")
					}
					runtimeMount, workMount := 0, 0
					for _, m := range c.VolumeMounts {
						if m.Name == "var-run-argo" {
							runtimeMount++
							if m.MountPath != "/var/run/argo" || !m.ReadOnly || m.SubPath != "" || m.SubPathExpr != "" {
								t.Fatalf("writable or aliased initializer runtime: %+v", m)
							}
						}
						if m.Name == "work" && m.MountPath == "/work" {
							workMount++
						}
					}
					if runtimeMount != 1 || workMount != 1 {
						t.Fatalf("runtime mounts=%d authored mounts=%d", runtimeMount, workMount)
					}
				}
				if !found {
					t.Fatal("authored initializer lost in merge")
				}
				for _, c := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
					if c.Name == "authored-init" {
						continue
					}
					for _, m := range c.VolumeMounts {
						if m.Name == "var-run-argo" && m.ReadOnly {
							t.Fatalf("changed runtime mount on %s", c.Name)
						}
					}
				}
				if err := validateExecutorTokenTarget(pod, defaultSourceSeedImage, false); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestExecutorIdentityRejectsWritableAuthoredInit(t *testing.T) {
	for _, restartable := range []bool{false, true} {
		t.Run(fmt.Sprintf("restartable=%v", restartable), func(t *testing.T) {
			pod := authoredInitMergedPod(t, "container", restartable)
			for i := range pod.Spec.InitContainers {
				if pod.Spec.InitContainers[i].Name == "authored-init" {
					for j := range pod.Spec.InitContainers[i].VolumeMounts {
						m := &pod.Spec.InitContainers[i].VolumeMounts[j]
						if m.Name == "var-run-argo" {
							m.ReadOnly = false
						}
					}
				}
			}
			if err := validateExecutorTokenTarget(pod, defaultSourceSeedImage, false); err == nil {
				t.Fatal("initializer can replace template before authority-bearing wait loads it")
			}
		})
	}
}
