package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"reflect"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestArgoUpgradePreservesOperatorValuesAndUsesNativeCap(t *testing.T) {
	stored := `{"controller":{"parallelism":3,"namespaceParallelism":2,"resources":{"requests":{"cpu":"100m","memory":"128Mi"}},"priorityClassName":"oberth-oberth-control-plane","config":{"artifactDrivers":[{"name":"operator"}],"archiveLogs":true}},"executor":{"resources":{"requests":{"cpu":"75m"}}},"extraObjects":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"operator-extra"}}]}`
	var applied []string
	deps := Deps{KubeClient: argoSecurityTestClient(), Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
		case "get":
			return []byte(stored), nil
		case "upgrade":
			applied = slices.Clone(args)
		}
		return nil, nil
	}}
	cap := 6
	if _, err := InstallArgoWorkflows(t.Context(), Config{Upgrade: true, ArgoParallelism: &cap}, deps); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(applied, "--reuse-values") || slices.Contains(applied, "--reset-values") || slices.Contains(applied, "--values") || !slices.Contains(applied, "controller.parallelism=6") {
		t.Fatal(applied)
	}
	var expected map[string]any
	if err := json.Unmarshal([]byte(stored), &expected); err != nil {
		t.Fatal(err)
	}
	objects := renderArgoHelmObjects(t, applied, expected)
	if !nativeArgoConfigHasCap(t, objects, 6) {
		t.Fatal("native controller caps changed")
	}
	wantController := expected["controller"].(map[string]any)
	foundController, foundExtra := false, false
	for _, object := range objects {
		meta, _ := object["metadata"].(map[string]any)
		foundExtra = foundExtra || object["kind"] == "ConfigMap" && meta["name"] == "operator-extra"
		if object["kind"] != "Deployment" {
			continue
		}
		foundController = true
		spec := object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		if spec["priorityClassName"] != wantController["priorityClassName"] {
			t.Fatal("operator controller priority changed")
		}
		controller := spec["containers"].([]any)[0].(map[string]any)
		resources := controller["resources"].(map[string]any)
		want := wantController["resources"].(map[string]any)
		if !reflect.DeepEqual(resources["requests"], want["requests"]) {
			t.Fatal("operator controller requests changed", resources)
		}
	}
	if !foundController || !foundExtra {
		t.Fatal("operator controller or extra object missing")
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
