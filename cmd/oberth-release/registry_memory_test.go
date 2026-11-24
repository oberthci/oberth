package main

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	corev1 "k8s.io/api/core/v1"
)

func TestRegistryLoginStaysOnPrivateMemory(t *testing.T) {
	wf, err := argoworkflow.Decode(ownWorkflow(t, "release"))
	if err != nil {
		t.Fatal(err)
	}
	dir, volumes, mounts := registryLoginStorage(wf)
	var selected corev1.VolumeMount
	for _, mount := range mounts {
		if strings.HasPrefix(dir, mount.MountPath+"/") && len(mount.MountPath) > len(selected.MountPath) {
			selected = mount
		}
	}
	volume := volumes[selected.Name]
	if selected.Name == "" || selected.Name == "work" || selected.Name == "cache" ||
		volume.EmptyDir == nil || volume.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatal("registry login is not on private memory", dir, selected.Name)
	}
}

func registryLoginStorage(wf *wfv1.Workflow) (string, map[string]corev1.Volume, map[string]corev1.VolumeMount) {
	volumes := map[string]corev1.Volume{}
	mounts := map[string]corev1.VolumeMount{}
	dir := ""
	for _, template := range []*wfv1.Template{wf.Spec.TemplateDefaults, wf.GetTemplateByName("release-images-pushed")} {
		for _, volume := range template.Volumes {
			volumes[volume.Name] = volume
		}
		for _, mount := range template.Container.VolumeMounts {
			mounts[mount.MountPath] = mount
		}
		for _, env := range template.Container.Env {
			if env.Name == "DOCKER_CONFIG" {
				dir = env.Value
			}
		}
	}
	return dir, volumes, mounts
}
