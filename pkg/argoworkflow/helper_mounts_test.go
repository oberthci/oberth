package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func helperMountWorkflow(t *testing.T, kind, target string) *wfv1.Workflow {
	t.Helper()
	wf, err := Decode([]byte(admissibleDocument))
	if err != nil {
		t.Fatal(err)
	}
	wf.Spec.Volumes = []corev1.Volume{{Name: "shadow", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: resource.NewQuantity(1<<20, resource.BinarySI)}}}}
	container := *wf.Spec.Templates[0].Container.DeepCopy()
	container.VolumeMounts = []corev1.VolumeMount{{Name: "shadow", MountPath: target, ReadOnly: true}}
	root := &wf.Spec.Templates[0]
	switch kind {
	case "container":
		root.Container = &container
	case "script":
		root.Container = nil
		root.Script = &wfv1.ScriptTemplate{Container: container, Source: "true"}
	case "set-member", "set-common":
		root.Container = nil
		member := container
		member.Name = "main"
		root.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: member}}}
		if kind == "set-common" {
			root.ContainerSet.VolumeMounts = member.VolumeMounts
			root.ContainerSet.Containers[0].VolumeMounts = nil
		}
	case "init":
		root.InitContainers = []wfv1.UserContainer{{Container: container}}
	case "sidecar":
		root.Sidecars = []wfv1.UserContainer{{Container: container}}
	case "defaults":
		wf.Spec.TemplateDefaults = &wfv1.Template{Container: &container}
	case "inline-dag":
		root.Container = nil
		root.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "child", Inline: &wfv1.Template{Container: &container}}}}
	case "inline-steps":
		root.Container = nil
		root.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "child", Inline: &wfv1.Template{Container: &container}}}}}
	default:
		t.Fatal(kind)
	}
	return wf
}

func TestAuthoredMountsCannotShadowServerHelpers(t *testing.T) {
	kinds := []string{"container", "script", "set-member", "set-common", "init", "sidecar", "defaults", "inline-dag", "inline-steps"}
	for _, kind := range kinds {
		for _, target := range []string{"/", "/run", "/run/oberth", "/run/oberth/assist", "/run/oberth/assist/oberth", "/run/oberth/assist/oberth/nested", "/run/oberth/another-helper", "/tmp/../run/oberth/assist/oberth", "//run/oberth/assist/", "/var", "/var/run", "/var/run/oberth/assist/oberth", "{{inputs.parameters.path}}", "/work/{{inputs.parameters.path}}"} {
			t.Run(kind+target, func(t *testing.T) {
				err := Admit(helperMountWorkflow(t, kind, target), Policy{})
				if err == nil || !strings.Contains(err.Error(), "server helper") {
					t.Fatalf("helper shadow admitted at %q: %v", target, err)
				}
			})
		}
		for _, target := range []string{"/run/oberth-other", "/run/oberth-gar", "/var/run/oberth-other", "/work/input"} {
			t.Run(kind+"allowed"+target, func(t *testing.T) {
				if err := Admit(helperMountWorkflow(t, kind, target), Policy{}); err != nil {
					t.Fatal("unrelated mount refused", err)
				}
			})
		}
	}
}

func TestAuthoredBlockDeviceCannotShadowServerHelper(t *testing.T) {
	source := strings.Replace(admissibleDocument, "  templates:\n", "  volumeClaimTemplates:\n  - metadata: {name: block}\n    spec:\n      accessModes: [ReadWriteOnce]\n      volumeMode: Block\n      resources:\n        requests: {storage: 1Gi}\n  templates:\n", 1)
	for _, target := range []string{"/run/oberth/assist/oberth", "/run/oberth", "/var/run/oberth/assist/oberth", "{{inputs.parameters.device}}", "/dev/work-disk"} {
		wf, err := Decode([]byte(source))
		if err != nil {
			t.Fatal(err)
		}
		wf.Spec.Templates[0].Container.VolumeDevices = []corev1.VolumeDevice{{Name: "block", DevicePath: target}}
		err = Admit(wf, Policy{})
		if target == "/dev/work-disk" {
			if err != nil {
				t.Fatal("ordinary block device refused", err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "server helper") {
			t.Fatalf("helper device shadow admitted at %q: %v", target, err)
		}
	}
}
