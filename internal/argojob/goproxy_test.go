package argojob

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/pipeline"
)

func TestBuildGoProxyAnnotation(t *testing.T) {
	for _, trigger := range []pipeline.Trigger{pipeline.TriggerCI, pipeline.TriggerRelease} {
		for _, mode := range []string{"default", "mirror-only", "invalid", "disabled"} {
			t.Run(string(trigger)+"/"+mode, func(t *testing.T) {
				config := testConfig()
				config.GoProxyURL = "https://mirror.example.invalid:8444"
				config.GoProxyModulePrefix = "go.example.test"
				config.GoProxyCACertPEM = testVaultAnchorPEM
				source := greedyDocument
				value := mode
				if mode == "disabled" {
					value = "mirror-only"
					config.GoProxyURL, config.GoProxyCACertPEM = "", ""
				}
				if mode != "default" {
					source = strings.Replace(source, "annotations:\n", "annotations:\n    oberth.ci/goproxy: "+value+"\n", 1)
				}
				workflow, err := Build(config, testRequest(trigger, source))
				if mode == "invalid" || mode == "disabled" {
					reason := `oberth.ci/goproxy must be "mirror-only" when present`
					if mode == "disabled" {
						reason = "oberth.ci/goproxy: mirror-only requires a configured Go module proxy"
					}
					if err == nil || !strings.Contains(err.Error(), reason) {
						t.Fatalf("expected refusal reason %q: %v", reason, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				want := config.GoProxyURL
				if mode == "default" {
					want += ",https://proxy.golang.org|direct"
				}
				found := 0
				for _, env := range workflow.Spec.Templates[0].Container.Env {
					if env.Name == "GOPROXY" {
						found++
						if env.Value != want || env.ValueFrom != nil {
							t.Fatalf("GOPROXY = %+v, want %q", env, want)
						}
					}
				}
				if found != 1 {
					t.Fatalf("GOPROXY appears %d times", found)
				}
			})
		}
	}
}

func TestGoProxyEnvironmentPreventsAmbientBypass(t *testing.T) {
	for _, trigger := range []pipeline.Trigger{pipeline.TriggerCI, pipeline.TriggerRelease} {
		for _, mode := range []string{"default", "mirror-only"} {
			t.Run(string(trigger)+"/"+mode, func(t *testing.T) {
				container := func() *corev1.Container {
					return &corev1.Container{Env: []corev1.EnvVar{
						{Name: "GOPROXY", Value: "direct"},
						{Name: "GONOPROXY", Value: "go.example.test/*"},
						{Name: "GOPRIVATE", Value: "go.example.test"},
						{Name: "GONOSUMDB", Value: "*"},
						{Name: "GOSUMDB", Value: "sum.golang.org"},
					}}
				}
				ordinary, inline, scriptContainer := container(), container(), container()
				script := &wfv1.ScriptTemplate{Container: *scriptContainer}
				workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Templates: []wfv1.Template{
					{Name: "ordinary", Container: ordinary},
					{Name: "script", Script: script},
					{Name: "steps", Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "inline", Inline: &wfv1.Template{Container: inline}}}}}},
				}}}
				if mode == "mirror-only" {
					workflow.Annotations = map[string]string{"oberth.ci/goproxy": mode}
				}
				config := testConfig()
				config.GoProxyURL = "https://mirror.example.invalid:8444"
				config.GoProxyModulePrefix = "go.example.test"
				injectRunEnvironment(workflow, config, testRequest(trigger, ""), nil)
				for _, c := range []*corev1.Container{ordinary, inline, &script.Container} {
					want := map[string]string{
						"GOPROXY":   config.GoProxyURL + ",https://proxy.golang.org|direct",
						"GONOPROXY": "none", "GONOSUMDB": "go.example.test",
						"GOPRIVATE": "go.example.test", "GOSUMDB": "sum.golang.org",
					}
					if mode == "mirror-only" {
						want["GOPROXY"] = config.GoProxyURL
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
}
