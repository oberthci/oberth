package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestArgoUpgradePreservesOperatorValuesAndUsesNativeCap(t *testing.T) {
	stored := `{"controller":{"parallelism":3,"namespaceParallelism":2,"resources":{"requests":{"cpu":"100m","memory":"128Mi"}},"priorityClassName":"oberth-oberth-control-plane","config":{"artifactDrivers":[{"name":"operator"}],"archiveLogs":true}},"executor":{"resources":{"requests":{"cpu":"75m"}}},"extraObjects":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"operator-extra"}}]}`
	var applied []string
	var filtered map[string]any
	deps := Deps{Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
		case "get":
			return []byte(stored), nil
		case "upgrade":
			applied = slices.Clone(args)
			index := slices.Index(args, "--values")
			if index < 0 || index+1 >= len(args) {
				t.Fatal("filtered dependency values absent", args)
			}
			body, err := os.ReadFile(args[index+1])
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body, &filtered); err != nil {
				t.Fatal(err)
			}
		}
		return nil, nil
	}}
	cap := 6
	if _, err := InstallArgoWorkflows(t.Context(), Config{Upgrade: true, ArgoParallelism: &cap}, deps); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applied, "--reset-values") || !slices.Contains(applied, "controller.parallelism=6") {
		t.Fatal(applied)
	}
	var expected map[string]any
	if err := json.Unmarshal([]byte(stored), &expected); err != nil {
		t.Fatal(err)
	}
	controller := expected["controller"].(map[string]any)
	delete(controller, "priorityClassName")
	delete(controller, "resources")
	if !reflect.DeepEqual(filtered, expected) {
		t.Fatal("unrelated operator values changed", filtered, expected)
	}
}

func TestNativeArgoChartRendersRunCaps(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed; the chart lane runs this render")
	}
	cap := 6
	args := []string{"template", "argo-workflows", "testdata/argo-workflows-1.0.24.tgz", "--set", "controller.namespaceParallelism=2"}
	installer := ArgoHelmArgs(Config{ArgoParallelism: &cap})
	for i := 0; i+1 < len(installer); i++ {
		if slices.Contains([]string{"--set", "--set-string", "--set-json"}, installer[i]) {
			args = append(args, installer[i], installer[i+1])
			i++
		}
	}
	rendered, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v: %s", err, rendered)
	}
	decoder := bytes.Split(rendered, []byte("\n---\n"))
	found := false
	for _, document := range decoder {
		var object struct {
			Kind string            `json:"kind"`
			Data map[string]string `json:"data"`
		}
		raw, err := yaml.YAMLToJSON(document)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		if object.Kind == "ConfigMap" {
			var config struct {
				Parallelism          int `json:"parallelism"`
				NamespaceParallelism int `json:"namespaceParallelism"`
			}
			if err := yaml.Unmarshal([]byte(object.Data["config"]), &config); err != nil {
				t.Fatal(err)
			}
			if config.Parallelism == 6 && config.NamespaceParallelism == 2 {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("native controller ConfigMap does not carry both run caps")
	}
}
