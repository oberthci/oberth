package installer

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func renderArgoHelmObjects(t *testing.T, flags []string, values map[string]any) []map[string]any {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is required in the chart lane")
	}
	file := filepath.Join(t.TempDir(), "values.json")
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"template", "argo-workflows", "testdata/argo-workflows-1.0.24.tgz", "--values", file}
	for i := 0; i+1 < len(flags); i++ {
		if slices.Contains([]string{"--set", "--set-string", "--set-json", "-n"}, flags[i]) {
			args = append(args, flags[i], flags[i+1])
			i++
		}
	}
	rendered, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v: %s", err, rendered)
	}
	var result []map[string]any
	for _, doc := range strings.Split(string(rendered), "\n---\n") {
		var object map[string]any
		if err := yaml.Unmarshal([]byte(doc), &object); err != nil {
			t.Fatal(err)
		}
		if object != nil {
			result = append(result, object)
		}
	}
	return result
}

func renderedArgoArgument(t *testing.T, objects []map[string]any, flag string) string {
	t.Helper()
	for _, object := range objects {
		if object["kind"] != "Deployment" {
			continue
		}
		raw, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		var deployment appsv1.Deployment
		if err := json.Unmarshal(raw, &deployment); err != nil {
			t.Fatal(err)
		}
		for _, c := range deployment.Spec.Template.Spec.Containers {
			for i, arg := range c.Args {
				if arg == flag && i+1 < len(c.Args) {
					return c.Args[i+1]
				}
			}
		}
	}
	t.Fatalf("missing controller argument %s", flag)
	return ""
}

func TestManagedArgoChartUsesTokenDeliveryExecutorImage(t *testing.T) {
	_, tag, _ := strings.Cut(argoworkflow.ExecutorImage, ":")
	for _, values := range []map[string]any{{}, {"executor": map[string]any{"image": map[string]any{"tag": tag}}}} {
		objects := renderArgoHelmObjects(t, ArgoHelmArgs(Config{}), values)
		if got := renderedArgoArgument(t, objects, "--executor-image"); got != argoworkflow.ExecutorImage {
			t.Fatalf("resource main token delivery rejects %q, want %q", got, argoworkflow.ExecutorImage)
		}
	}
}

func TestManagedArgoExecutorImagePreservesMatchingAndRefusesConflictingValues(t *testing.T) {
	_, tag, _ := strings.Cut(argoworkflow.ExecutorImage, ":")
	for _, test := range []struct {
		name, stored           string
		wantMissing, wantError bool
	}{
		{"absent", `{}`, true, false},
		{"matching", `{"executor":{"image":{"tag":"` + tag + `"},"resources":{"requests":{"cpu":"75m"}}}}`, false, false},
		{"tag", `{"executor":{"image":{"tag":"v4.0.8"}}}`, false, true},
		{"repository", `{"executor":{"image":{"repository":"operator/argoexec"}}}`, false, true},
		{"registry", `{"executor":{"image":{"registry":"mirror.invalid"}}}`, false, true},
		{"empty-tag", `{"executor":{"image":{"tag":""}}}`, false, true},
		{"null-parent", `{"executor":null}`, false, true},
		{"invalid", `{} {}`, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			writes := 0
			deps := Deps{KubeClient: argoSecurityTestClient(), Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "get":
					return []byte(test.stored), nil
				case "list":
					return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
				case "upgrade":
					writes++
				}
				return nil, nil
			}}
			missing, err := checkArgoExecutorImage(t.Context(), deps, "pipelines", true)
			if (err != nil) != test.wantError || missing != test.wantMissing {
				t.Fatal(missing, err)
			}
			if (test.name == "repository" || test.name == "registry") && (err == nil || !strings.Contains(err.Error(), "--skip-argo-workflows")) {
				t.Fatal("operator mirror remedy absent", err)
			}
			_, err = InstallArgoWorkflows(t.Context(), Config{Upgrade: true}, deps)
			if (err != nil) != test.wantError || (test.wantError && writes != 0) {
				t.Fatal("operator image overwritten", writes, err)
			}
		})
	}
}

func TestManagedArgoReconcilesMissingExecutorPinWithoutChartUpgrade(t *testing.T) {
	stored := `{}`
	writes := 0
	deps := Deps{KubeClient: argoSecurityTestClient(), Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
		case "get":
			return []byte(stored), nil
		case "upgrade":
			writes++
			stored = storedArgoExecutorOverrides(t, stored, args)
		}
		return nil, nil
	}}
	result, err := InstallArgoWorkflows(t.Context(), Config{}, deps)
	if err != nil || !result.Upgraded || writes != 1 {
		t.Fatal(result, writes, err)
	}
	result, err = InstallArgoWorkflows(t.Context(), Config{}, deps)
	if err != nil || !result.AlreadyInstalled || writes != 1 {
		t.Fatal(result, writes, err)
	}
}

// Model Helm's persisted executor and main settings without discarding unrelated values.
func storedArgoExecutorOverrides(t *testing.T, stored string, args []string) string {
	t.Helper()
	var values map[string]any
	if err := json.Unmarshal(nonEmptyJSON([]byte(stored)), &values); err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--set-string" && args[i] != "--set-json" {
			continue
		}
		key, raw, ok := strings.Cut(args[i+1], "=")
		if !ok || (!strings.HasPrefix(key, "executor.") && !strings.HasPrefix(key, "mainContainer.") && !strings.HasPrefix(key, "controller.") && !strings.HasPrefix(key, "server.")) {
			continue
		}
		var value any = raw
		if args[i] == "--set-json" {
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				t.Fatal(err)
			}
		}
		fields := strings.Split(key, ".")
		object := values
		for _, field := range fields[:len(fields)-1] {
			child, ok := object[field].(map[string]any)
			if !ok {
				child = map[string]any{}
				object[field] = child
			}
			object = child
		}
		object[fields[len(fields)-1]] = value
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
