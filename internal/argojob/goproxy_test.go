package argojob

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestGoProxyEnvironmentPreventsAmbientBypass(t *testing.T) {
	for _, trigger := range []periapsis.Trigger{periapsis.TriggerCI, periapsis.TriggerRelease} {
		t.Run(string(trigger), func(t *testing.T) {
			container := func() *corev1.Container {
				return &corev1.Container{Env: []corev1.EnvVar{
					{Name: "GOPROXY", Value: "direct"},
					{Name: "GONOPROXY", Value: "go.example.test/*"},
					{Name: "GOPRIVATE", Value: "go.example.test"},
					{Name: "GONOSUMDB", Value: "*"},
					{Name: "GOSUMDB", Value: "sum.golang.org"},
				}}
			}
			ordinary, inline := container(), container()
			workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{
				{Name: "ordinary", Container: ordinary},
				{Name: "steps", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "inline", Inline: &wfv1.Template{Container: inline}}}}}},
			}}}
			config := testConfig()
			config.GoProxyURL = "https://mirror.example.invalid:8444"
			config.GoProxyModulePrefix = "go.example.test"
			injectRunEnvironment(workflow, config, testRequest(trigger, ""), false)
			for _, c := range []*corev1.Container{ordinary, inline} {
				want := map[string]string{
					"GOPROXY":   config.GoProxyURL + ",https://proxy.golang.org|direct",
					"GONOPROXY": "none", "GONOSUMDB": "go.example.test",
					"GOPRIVATE": "go.example.test", "GOSUMDB": "sum.golang.org",
				}
				for name, value := range want {
					count := 0
					for _, env := range c.Env {
						if env.Name == name {
							count++
							if env.Value != value {
								t.Errorf("%s = %q, want %q", name, env.Value, value)
							}
						}
					}
					if count != 1 {
						t.Errorf("%s appears %d times", name, count)
					}
				}
			}
		})
	}
}
