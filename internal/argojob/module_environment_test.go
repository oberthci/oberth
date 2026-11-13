package argojob

import (
	"reflect"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestModuleEnvironmentOwnedByRepository(t *testing.T) {
	for _, trigger := range []pipeline.Trigger{pipeline.TriggerCI, pipeline.TriggerRelease} {
		for _, authored := range []bool{false, true} {
			t.Run(string(trigger)+"/authored="+map[bool]string{false: "false", true: "true"}[authored], func(t *testing.T) {
				var expected []corev1.EnvVar
				if authored {
					expected = []corev1.EnvVar{
						{Name: "GOPROXY", Value: "direct"},
						{Name: "GONOPROXY", Value: "go.example.test/*"},
						{Name: "GONOSUMDB", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "module-access"}, Key: "no-sumdb"}}},
					}
				}
				container := func() *corev1.Container { return &corev1.Container{Env: append([]corev1.EnvVar(nil), expected...)} }
				ordinary, inline, scriptContainer := container(), container(), container()
				script := &wfv1.ScriptTemplate{Container: *scriptContainer}
				workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{
					{Name: "ordinary", Container: ordinary},
					{Name: "script", Script: script},
					{Name: "steps", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "inline", Inline: &wfv1.Template{Container: inline}}}}}},
				}}}
				injectRunEnvironment(workflow, testConfig(), testRequest(trigger, ""), false)
				for _, c := range []*corev1.Container{ordinary, inline, &script.Container} {
					var actual []corev1.EnvVar
					for _, env := range c.Env {
						if env.Name == "GOPROXY" || env.Name == "GONOPROXY" || env.Name == "GONOSUMDB" {
							actual = append(actual, env)
						}
					}
					if !reflect.DeepEqual(actual, expected) {
						t.Fatalf("repository module environment changed: got %+v, want %+v", actual, expected)
					}
				}
			})
		}
	}
}
