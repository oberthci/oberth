package installer

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// This models the explicit operator edit documented in the release notes. There
// is no automatic profile-value deletion or ownership guess in the installer.
func TestOldArgoProfileNeedsExplicitNativeValuesMigration(t *testing.T) {
	const oldName = "old-profile"
	profileVolume := map[string]any{"name": "oberth-controller-profile", "configMap": map[string]any{"name": oldName}}
	operatorVolume := map[string]any{"name": "operator-volume", "emptyDir": map[string]any{}}
	profileMount := map[string]any{"name": "oberth-controller-profile", "mountPath": "/var/run/oberth-controller-profile"}
	operatorMount := map[string]any{"name": "operator-volume", "mountPath": "/operator"}
	profileObject := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": oldName}, "data": map[string]any{"config": "parallelism: 3"}}
	operatorObject := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "operator-extra"}, "data": map[string]any{"key": "value"}}
	controller := map[string]any{"configMap": map[string]any{"create": false, "name": oldName}, "parallelism": 3, "namespaceParallelism": 2, "volumes": []any{profileVolume, operatorVolume}, "volumeMounts": []any{profileMount, operatorMount}, "image": map[string]any{"tag": "v4.0.8@sha256:7a156419f80285859fc8f859927b6cc249f0d128e161e080dc17fca8fcbbceb6"}}
	values := map[string]any{"controller": controller, "extraObjects": []any{profileObject, operatorObject}}
	cap := 6
	flags := ArgoHelmArgs(Config{ArgoParallelism: &cap})
	before := renderArgoHelmObjects(t, flags, values)
	if got := renderedArgoArgument(t, before, "--configmap"); got != oldName {
		t.Fatal(got)
	}
	if nativeArgoConfigHasCap(t, before, 6) {
		t.Fatal("reuse-values unexpectedly enabled native cap")
	}
	delete(controller, "configMap")
	controller["volumes"] = []any{operatorVolume}
	controller["volumeMounts"] = []any{operatorMount}
	delete(controller["image"].(map[string]any), "tag")
	values["extraObjects"] = []any{operatorObject}
	after := renderArgoHelmObjects(t, flags, values)
	if got := renderedArgoArgument(t, after, "--configmap"); got == oldName {
		t.Fatal("old profile reference survived")
	}
	if !nativeArgoConfigHasCap(t, after, 6) {
		t.Fatal("native ConfigMap/cap absent after documented migration")
	}
	foundOperatorObject, foundVolume, foundMount := false, false, false
	for _, object := range after {
		meta, _ := object["metadata"].(map[string]any)
		if object["kind"] == "ConfigMap" && meta["name"] == "operator-extra" {
			foundOperatorObject = true
		}
		if object["kind"] != "Deployment" {
			continue
		}
		spec := object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		volumes := spec["volumes"].([]any)
		foundVolume = slices.ContainsFunc(volumes, func(v any) bool { return v.(map[string]any)["name"] == "operator-volume" })
		for _, c := range spec["containers"].([]any) {
			mounts, _ := c.(map[string]any)["volumeMounts"].([]any)
			foundMount = foundMount || slices.ContainsFunc(mounts, func(v any) bool { return v.(map[string]any)["name"] == "operator-volume" })
		}
	}
	if !foundOperatorObject || !foundVolume || !foundMount {
		t.Fatal("unrelated operator entries lost", foundOperatorObject, foundVolume, foundMount)
	}
}

func nativeArgoConfigHasCap(t *testing.T, objects []map[string]any, want int) bool {
	t.Helper()
	for _, object := range objects {
		if object["kind"] != "ConfigMap" {
			continue
		}
		data, _ := object["data"].(map[string]any)
		config, _ := data["config"].(string)
		var parsed struct {
			Parallelism          int `json:"parallelism"`
			NamespaceParallelism int `json:"namespaceParallelism"`
		}
		raw, err := yaml.YAMLToJSON([]byte(config))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.Parallelism == want && parsed.NamespaceParallelism == 2 {
			return true
		}
	}
	return false
}

func TestOldArgoProfileExecutorResourcesNoticeWithoutParallelism(t *testing.T) {
	for _, test := range []struct {
		name      string
		resources any
		label     string
	}{
		{"missing", nil, "installer defaults"},
		{"stored", map[string]any{"requests": map[string]any{"cpu": "75m"}}, "stored value kept"},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]any{"controller": map[string]any{"configMap": map[string]any{"create": false, "name": "old-profile"}}}
			if test.resources != nil {
				values["executor"] = map[string]any{"resources": test.resources}
			}
			raw, err := json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			deps := Deps{KubeClient: argoSecurityTestClient(), Output: &output, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "list":
					return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
				case "get":
					return raw, nil
				case "upgrade":
					for _, arg := range args {
						if test.resources != nil && strings.HasPrefix(arg, "executor.resources=") {
							t.Fatal("stored resources replaced", args)
						}
					}
				}
				return nil, nil
			}}
			if _, err := InstallArgoWorkflows(t.Context(), Config{}, deps); err != nil {
				t.Fatal(err)
			}
			text := output.String()
			if strings.Count(text, "controller.configMap.create=false:") != 1 || !strings.Contains(text, "executor.resources") || !strings.Contains(text, "docs/releases/v0.22.5.md") || !strings.Contains(text, test.label) {
				t.Fatal("old profile silently ignored native resources", text)
			}
		})
	}
}
