package argojob

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestBuildRefusesDefaultGraphInlineMountBypass(t *testing.T) {
	for _, kind := range []string{"dag", "steps"} {
		for _, volume := range []string{ReleaseTokenVolumeName, SecretsVolumeName, SourceVolumeName} {
			t.Run(kind+"/"+volume, func(t *testing.T) {
				request := isolationRequest(t, func(wf *wfv1.Workflow) {
					leaf := wf.Spec.Templates[0].DeepCopy()
					leaf.Name = "run"
					injected := &wfv1.Template{Container: &corev1.Container{
						Image: leaf.Container.Image, Command: []string{"/bin/true"},
						VolumeMounts: []corev1.VolumeMount{{Name: volume, MountPath: "/stolen", ReadOnly: false}},
					}}
					root := wfv1.Template{Name: "main"}
					defaults := &wfv1.Template{}
					if kind == "dag" {
						root.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "run", Template: "run"}}}
						defaults.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "injected", Inline: injected}}}
					} else {
						root.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "run", Template: "run"}}}}
						defaults.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "injected", Inline: injected}}}}
					}
					wf.Spec.Templates = []wfv1.Template{root, *leaf}
					wf.Spec.TemplateDefaults = defaults
				})
				wf, err := Build(testConfig(), request)
				if err == nil || !strings.Contains(err.Error(), "spec.templateDefaults."+kind+" is not admitted") {
					t.Fatalf("Build returned workflow=%t, err=%v; want default graph refusal", wf != nil, err)
				}
			})
		}
	}
}

func TestBuildRefusesDefaultHelpers(t *testing.T) {
	for _, kind := range []string{"sidecars", "initContainers"} {
		t.Run(kind, func(t *testing.T) {
			request := isolationRequest(t, func(wf *wfv1.Workflow) {
				helper := wfv1.UserContainer{Container: corev1.Container{Name: "unprotected", Image: wf.Spec.Templates[0].Container.Image, Command: []string{"/bin/true"}}}
				wf.Spec.TemplateDefaults = &wfv1.Template{}
				if kind == "sidecars" {
					wf.Spec.TemplateDefaults.Sidecars = []wfv1.UserContainer{helper}
				} else {
					wf.Spec.TemplateDefaults.InitContainers = []wfv1.UserContainer{helper}
				}
			})
			wf, err := Build(testConfig(), request)
			if err == nil || !strings.Contains(err.Error(), "spec.templateDefaults."+kind+" is not admitted") {
				t.Fatalf("Build returned workflow=%t, err=%v; want default helper refusal", wf != nil, err)
			}
		})
	}
}

func TestDefaultVolumeDevicesRefused(t *testing.T) {
	for _, kind := range []string{"container", "script", "containerSet", "sidecars", "initContainers"} {
		t.Run(kind, func(t *testing.T) {
			request := isolationRequest(t, func(wf *wfv1.Workflow) {
				main := *wf.Spec.Templates[0].Container.DeepCopy()
				main.Name = "main"
				device := corev1.Container{Name: "main", Image: main.Image, VolumeDevices: []corev1.VolumeDevice{{Name: SourceVolumeName, DevicePath: "/dev/stolen"}}}
				defaults := &wfv1.Template{}
				switch kind {
				case "container":
					defaults.Container = &device
				case "script":
					wf.Spec.Templates[0].Container = nil
					wf.Spec.Templates[0].Script = &wfv1.ScriptTemplate{Container: main, Source: "true"}
					defaults.Script = &wfv1.ScriptTemplate{Container: device}
				case "containerSet":
					wf.Spec.Templates[0].Container = nil
					wf.Spec.Templates[0].ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: main}}}
					defaults.ContainerSet = &wfv1.ContainerSetTemplate{Containers: []wfv1.ContainerNode{{Container: device}}}
				case "sidecars":
					defaults.Sidecars = []wfv1.UserContainer{{Container: device}}
				case "initContainers":
					defaults.InitContainers = []wfv1.UserContainer{{Container: device}}
				}
				wf.Spec.TemplateDefaults = defaults
			})
			t.Run("Build", func(t *testing.T) {
				if wf, err := Build(testConfig(), request); err == nil {
					t.Fatalf("Build admitted a default %s device for %s: workflow=%t", kind, SourceVolumeName, wf != nil)
				}
			})
			t.Run("document-boundary", func(t *testing.T) {
				wf, err := argoworkflow.Decode(request.Source)
				if err != nil {
					t.Fatal(err)
				}
				// Test the mount/device boundary independently: helper defaults
				// also face their own earlier structural admission refusal.
				if err := ValidateDocument(wf, request.Trigger); err == nil || !strings.Contains(err.Error(), "volumeDevices") {
					t.Fatal("default device escaped document protection", err)
				}
			})
		})
	}
}
