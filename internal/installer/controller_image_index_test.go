package installer_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/chart/loader"
	"sigs.k8s.io/yaml"
)

// The pinned controller takes its image index and workflow defaults from its ConfigMap
// only at start, so both are part of its Pod template.
func TestControllerRollsExactlyWhenItsStartOnlyConfigurationChanges(t *testing.T) {
	t.Parallel()
	first := controllerTemplate(t, "sha256:"+strings.Repeat("1", 64))
	again := controllerTemplate(t, "sha256:"+strings.Repeat("1", 64))
	other := controllerTemplate(t, "sha256:"+strings.Repeat("2", 64))
	if first != again {
		t.Fatal("same release renders different controller Pod templates")
	}
	if first == other {
		t.Fatal("a changed source plugin image leaves the controller Pod template unchanged")
	}
	defaults := map[string]any{"spec": map[string]any{"ttlStrategy": map[string]any{"secondsAfterCompletion": 60}}}
	withDefaults := controllerTemplate(t, "sha256:"+strings.Repeat("1", 64), defaults)
	if withDefaults == first || withDefaults != controllerTemplate(t, "sha256:"+strings.Repeat("1", 64), defaults) {
		t.Fatal("changed workflow defaults do not roll the controller exactly once")
	}
}

// controllerTemplate renders the chart for one source plugin digest and,
// optionally, workflow defaults, and returns the controller Deployment's Pod
// template after checking the ConfigMap indexes the digest.
func controllerTemplate(t *testing.T, digest string, defaults ...map[string]any) string {
	t.Helper()
	ch, err := loader.Load(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"sourcePluginImageDigest": digest}
	if len(defaults) > 0 {
		values["argoNative"] = map[string]any{"controller": map[string]any{"workflowDefaults": defaults[0]}}
	}
	files, err := installer.Render(ch, values, "oberth")
	if err != nil {
		t.Fatal(err)
	}
	template, indexed := "", false
	for name, raw := range files {
		if strings.Contains(raw, "kind: ConfigMap") && strings.Contains(raw, "  images: |") {
			indexed = strings.Contains(raw, digest)
		}
		if strings.Contains(name, "argoNative") && strings.Contains(raw, "kind: Deployment") {
			// WatchController reads this Deployment by name.
			if !strings.Contains(raw, "\n  name: oberth-controller-workflow-controller\n") {
				t.Fatal("controller Deployment renamed")
			}
			template = podTemplate(t, raw)
		}
	}
	if template == "" || !indexed {
		t.Fatal("controller Deployment or its image index missing", indexed)
	}
	return template
}

func podTemplate(t *testing.T, deployment string) string {
	t.Helper()
	var d struct {
		Spec struct {
			Template map[string]any `json:"template"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(deployment), &d); err != nil {
		t.Fatal(err)
	}
	out, err := yaml.Marshal(d.Spec.Template)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
