package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestArgoExecutorResourcesPreservesEveryOperatorChoice(t *testing.T) {
	for _, raw := range []string{
		`{"executor":{"resources":{"requests":{"cpu":"75m"}}}}`,
		`{"executor":{"resources":{}}}`,
		`{"executor":{"resources":null}}`,
		`{"executor":{"resources":{"limits":{"ephemeral-storage":"2Gi"},"claims":[{"name":"operator"}]}}}`,
		`{"executor":null}`,
	} {
		t.Run(raw, func(t *testing.T) {
			deps := Deps{RunHelm: func(context.Context, []string) ([]byte, error) { return []byte(raw), nil }}
			review, err := readArgoExecutorResources(t.Context(), deps, "pipelines", true)
			if err != nil {
				t.Fatal(err)
			}
			if review == nil || !review.OperatorSet {
				t.Fatal("operator choice lost", review)
			}
			var expected map[string]any
			if err := json.Unmarshal([]byte(raw), &expected); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(review.Resources, valueAt(expected, "executor", "resources")) {
				t.Fatal("operator resources were rewritten")
			}
			config := Config{argoExecutorResources: review}
			var text strings.Builder
			PrintReview(&text, config)
			if !strings.Contains(text.String(), "keeping the operator-set value") {
				t.Fatal("review hides preserved resources", text.String())
			}
			if Review(config).ArgoExecutorResources != review {
				t.Fatal("JSON review lost the operator setting")
			}
		})
	}
}

func TestArgoExecutorDefaultsReconcileAnExistingReleaseOnlyOnce(t *testing.T) {
	stored := `{}`
	writes := 0
	var output strings.Builder
	deps := Deps{Output: &output, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
		case "get":
			return []byte(stored), nil
		case "upgrade":
			writes++
			found := false
			for _, arg := range args {
				if strings.HasPrefix(arg, "executor.resources=") {
					found = true
				}
				if arg == "--reset-values" {
					t.Fatal("native defaults reset operator values")
				}
			}
			if !found {
				t.Fatal("native executor defaults missing")
			}
			stored = storedArgoExecutorOverrides(t, stored, args)
		}
		return nil, nil
	}}
	first, err := InstallArgoWorkflows(t.Context(), Config{}, deps)
	if err != nil || !first.Upgraded || writes != 1 {
		t.Fatal(first, writes, err)
	}
	second, err := InstallArgoWorkflows(t.Context(), Config{}, deps)
	if err != nil || !second.AlreadyInstalled || writes != 1 {
		t.Fatal(second, writes, err)
	}
	if !strings.Contains(output.String(), "installer defaults") || !strings.Contains(output.String(), "keeping the operator-set value") {
		t.Fatal(output.String())
	}
}

func TestArgoExecutorResourcesReadFailureDoesNotApplyDefaults(t *testing.T) {
	for _, raw := range []string{`{"executor":`, `{} {"executor":{"resources":{}}}`} {
		writes := 0
		deps := Deps{Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "list":
				return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
			case "get":
				return []byte(raw), nil
			case "upgrade":
				writes++
			}
			return nil, nil
		}}
		if _, err := InstallArgoWorkflows(t.Context(), Config{Upgrade: true}, deps); err == nil || writes != 0 {
			t.Fatal("unreadable operator settings were overwritten", err, writes)
		}
	}
}

func TestArgoExecutorReviewShowsDefaultsAndOmitsUnmanagedController(t *testing.T) {
	var text strings.Builder
	PrintReview(&text, Config{})
	if !strings.Contains(text.String(), "Argo executor.resources: installer defaults") {
		t.Fatal(text.String())
	}
	text.Reset()
	PrintReview(&text, Config{SkipArgo: true})
	if strings.Contains(text.String(), "Argo executor.resources") || Review(Config{SkipArgo: true}).ArgoExecutorResources != nil {
		t.Fatal("unmanaged controller was given installer defaults")
	}
}

func TestArgoChartRendersNativeExecutorResources(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed; the chart lane renders native executor.resources")
	}
	for _, operator := range []any{nil, map[string]any{"requests": map[string]any{"cpu": "75m"}}, map[string]any{}} {
		cfg := Config{}
		args := []string{"template", "argo-workflows", "testdata/argo-workflows-1.0.24.tgz"}
		expected := argoworkflow.ExecutorResources()
		if operator != nil {
			cfg.argoExecutorResources = &ArgoExecutorResourcesReview{OperatorSet: true, Resources: operator}
			raw, _ := json.Marshal(operator)
			args = append(args, "--set-json", "executor.resources="+string(raw))
			// Unmarshal into a fresh value: the operator's partial map is not completed.
			expected = corev1.ResourceRequirements{}
			if err := json.Unmarshal(raw, &expected); err != nil {
				t.Fatal(err)
			}
		}
		managed := ArgoHelmArgs(cfg)
		for i := 0; i+1 < len(managed); i++ {
			if slices.Contains([]string{"--set", "--set-string", "--set-json"}, managed[i]) {
				args = append(args, managed[i], managed[i+1])
				i++
			}
		}
		rendered, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("Helm: %v: %s", err, rendered)
		}
		found := false
		for _, document := range bytes.Split(rendered, []byte("\n---\n")) {
			var object struct {
				Kind string            `json:"kind"`
				Data map[string]string `json:"data"`
			}
			if err := yaml.Unmarshal(document, &object); err != nil {
				t.Fatal(err)
			}
			if object.Kind != "ConfigMap" {
				continue
			}
			var config struct {
				Executor struct {
					Resources corev1.ResourceRequirements `json:"resources"`
				} `json:"executor"`
			}
			if err := yaml.Unmarshal([]byte(object.Data["config"]), &config); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(config.Executor.Resources, expected) {
				t.Fatalf("native resources %#v, want %#v", config.Executor.Resources, expected)
			}
			found = true
		}
		if !found {
			t.Fatal("native controller ConfigMap missing")
		}
	}
}
