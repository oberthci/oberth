package controller

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/argoproj/argo-workflows/v4/config"
	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo-workflows/v4/workflow/common"
	corev1 "k8s.io/api/core/v1"
)

func addControllerSource(wf *wfv1.Workflow, name string) {
	if name != "own-build-structure" {
		return
	}
	for i := range wf.Spec.Templates {
		tmpl := &wf.Spec.Templates[i]
		if tmpl.Container != nil {
			tmpl.Inputs.Artifacts = append(tmpl.Inputs.Artifacts, wfv1.Artifact{Name: "source", Path: "/work/src", ArtifactLocation: wfv1.ArtifactLocation{Plugin: &wfv1.PluginArtifact{Name: "oberth", Key: "run-1"}}})
		}
	}
}

func configureControllerSource(controller *WorkflowController) {
	controller.Config.ArtifactDrivers = []config.ArtifactDriver{{Name: "oberth", Image: "source-image"}}
	controller.Config.Images["source-image"] = config.Image{Entrypoint: []string{"/usr/local/bin/oberth-source-plugin"}}
	controller.Config.Executor = &corev1.Container{Env: []corev1.EnvVar{
		{Name: "OBERTH_SOURCE_ENDPOINT", Value: "https://source.test"},
		{Name: "OBERTH_SOURCE_SERVER_NAME", Value: "source.test"},
		{Name: "OBERTH_SOURCE_CA_PEM", Value: "public-ca"},
	}}
}

// Inspect created Pods after the controller adds artifact/runtime mounts and
// applies the actual preparation patch supplied by the native execution test.
func checkControllerSourcePod(t *testing.T, pod *corev1.Pod) bool {
	t.Helper()
	for _, plugin := range pod.Spec.InitContainers {
		if plugin.Name != common.ArtifactPluginInitPrefix+"oberth" {
			continue
		}
		if plugin.Image != "source-image" || !slices.Contains(plugin.Command, "/usr/local/bin/oberth-source-plugin") || !slices.Contains(plugin.Command, wfv1.ArtifactPluginName("oberth").SocketPath()) {
			t.Fatal("source driver image or command differs")
		}
		for _, mount := range []corev1.VolumeMount{
			{Name: common.ServiceAccountTokenVolumeName, MountPath: common.ServiceAccountTokenMountPath, ReadOnly: true},
			{Name: "var-run-argo", MountPath: common.VarRunArgoPath},
			{Name: "input-artifacts", MountPath: common.ExecutorArtifactBaseDir},
		} {
			if !slices.Contains(plugin.VolumeMounts, mount) {
				t.Fatal("source driver lacks final Pod mount", mount.MountPath)
			}
		}
		for _, expected := range []corev1.EnvVar{{Name: "OBERTH_SOURCE_ENDPOINT", Value: "https://source.test"}, {Name: "OBERTH_SOURCE_SERVER_NAME", Value: "source.test"}, {Name: "OBERTH_SOURCE_CA_PEM", Value: "public-ca"}} {
			if !slices.Contains(plugin.Env, expected) {
				t.Fatal("source driver lacks executor environment", expected.Name)
			}
		}
		if os.Getenv("OBERTH_CONTROLLER_POD_PATCH") != "" {
			checkControllerSourceProjection(t, pod)
		}
		return true
	}
	return false
}

func checkControllerSourceProjection(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	for _, volume := range pod.Spec.Volumes {
		if volume.Name != common.ServiceAccountTokenVolumeName {
			continue
		}
		if volume.Secret != nil || volume.Projected == nil {
			t.Fatal("source token volume lacks the preparation projection")
		}
		for _, source := range volume.Projected.Sources {
			if token := source.ServiceAccountToken; token != nil {
				if filepath.Join(common.ServiceAccountTokenMountPath, token.Path) != "/var/run/secrets/kubernetes.io/serviceaccount/token" || token.Audience != "" {
					t.Fatal("source plugin path or API server audience differs in the created Pod")
				}
				return
			}
		}
	}
	t.Fatal("created source Pod lacks its projected token")
}
