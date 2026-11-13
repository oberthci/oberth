package argojob

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
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
