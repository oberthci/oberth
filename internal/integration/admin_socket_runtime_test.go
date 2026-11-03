package integration_test

import (
	"os/exec"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestAdminSocketHasWritablePrivateRuntimeVolume(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required to render the chart")
	}
	output, err := exec.Command(helm, "template", "oberth", "../../charts/oberth", "--show-only", "templates/deployment.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("render chart: %v %s", err, output)
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(output, &deployment); err != nil {
		t.Fatal(err)
	}
	pod := deployment.Spec.Template.Spec
	for _, volume := range pod.Volumes {
		if volume.Name != "admin-run" {
			continue
		}
		if volume.EmptyDir == nil {
			t.Fatal("admin runtime must use a dedicated writable emptyDir")
		}
		mounted := 0
		for _, container := range append(pod.Containers, pod.InitContainers...) {
			for _, mount := range container.VolumeMounts {
				if mount.Name == volume.Name && mount.MountPath == "/run/oberth" && !mount.ReadOnly {
					mounted++
					if container.Name == "prepare-caches" && !slices.Contains(container.Args, "/run/oberth") {
						t.Fatal("init does not prepare ownership of the runtime directory")
					}
				}
			}
		}
		if mounted != 2 {
			t.Fatalf("runtime directory must be shared by ownership init and daemon: %d mounts", mounted)
		}
		return
	}
	t.Fatal("daemon has no writable admin runtime volume")
}
