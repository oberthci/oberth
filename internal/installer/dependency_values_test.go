package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"slices"
	"testing"
)

func TestDependencyUpgradeDropsManagedSchedulingAndKeepsOperatorValues(t *testing.T) {
	for _, test := range []struct{ release, component, chart, kind string }{{"argo-workflows", "controller", "testdata/argo-workflows-1.0.24.tgz", "Deployment"}, {"openbao", "server", "testdata/openbao-0.28.6.tgz", "StatefulSet"}} {
		t.Run(test.release, func(t *testing.T) {
			stored := map[string]any{test.component: map[string]any{"priorityClassName": "oberth-oberth-control-plane", "resources": map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "128Mi"}}, "nodeSelector": map[string]any{"pool": "operator"}}, "operatorMarker": json.Number("9007199254740993")}
			if test.component == "controller" {
				stored[test.component].(map[string]any)["parallelism"] = 2
				stored[test.component].(map[string]any)["namespaceParallelism"] = 4
			}
			raw, _ := json.Marshal(stored)
			deps := Deps{RunHelm: func(context.Context, []string) ([]byte, error) { return raw, nil }}
			file, changed, cleanup, err := dependencySchedulingValues(t.Context(), deps, test.release, "pipelines", test.component, controlPlanePriority(Config{}), true)
			defer cleanup()
			if err != nil || !changed {
				t.Fatal(changed, err)
			}
			cfg := Config{InstallSecretStoreDev: true}
			args := ArgoHelmArgs(cfg)
			if test.release == "openbao" {
				args = OpenBaoHelmArgs(cfg)
			}
			args = withDependencyValues(args, file)
			if slices.Contains(args, "--reuse-values") || !slices.Contains(args, "--reset-values") {
				t.Fatal("stored overrides can survive", args)
			}
			renderDependencyPods(t, cfg, test.chart, args, test.kind, "")
			body, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			decoder := json.NewDecoder(bytes.NewReader(body))
			decoder.UseNumber()
			if err := decoder.Decode(&got); err != nil {
				t.Fatal(err)
			}
			component := got[test.component].(map[string]any)
			if !reflect.DeepEqual(component["nodeSelector"], map[string]any{"pool": "operator"}) || got["operatorMarker"] != json.Number("9007199254740993") {
				t.Fatal("operator configuration changed", got)
			}
			if test.component == "controller" && (component["parallelism"] != json.Number("2") || component["namespaceParallelism"] != json.Number("4")) {
				t.Fatal("global cap lost", got)
			}
		})
	}
}
func TestDependencyUpgradeActuallyReconcilesStoredScheduling(t *testing.T) {
	for _, release := range []string{"argo-workflows", "openbao"} {
		t.Run(release, func(t *testing.T) {
			var calls [][]string
			deps := argoTestDeps(t, &calls)
			deps.Output = io.Discard
			component, version := "controller", DefaultArgoChartVersion
			if release == "openbao" {
				component = "server"
				version = DefaultOpenBaoChartVersion
			}
			applied := false
			deps.RunHelm = func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "list":
					return json.Marshal([]map[string]string{{"name": release, "chart": release + "-" + version, "status": "deployed"}})
				case "get":
					return json.Marshal(map[string]any{component: map[string]any{"priorityClassName": "oberth-oberth-control-plane", "resources": map[string]any{"requests": map[string]any{"cpu": "100m", "memory": "128Mi"}}}})
				case "upgrade":
					applied = true
					if !slices.Contains(args, "--reset-values") {
						t.Fatal(args)
					}
					i := slices.Index(args, "--values")
					if i < 0 {
						t.Fatal("filtered values absent")
					}
					if _, err := os.Stat(args[i+1]); err != nil {
						t.Fatal(err)
					}
				}
				return nil, nil
			}
			cfg := Config{InstallSecretStoreDev: true}
			var err error
			if release == "openbao" {
				_, err = InstallOpenBao(t.Context(), cfg, deps)
			} else {
				_, err = InstallArgoWorkflows(t.Context(), cfg, deps)
			}
			if err != nil || !applied {
				t.Fatal("stored scheduling not removed", applied, err)
			}
		})
	}
}

func TestDependencySchedulingPreservesOperatorOverrides(t *testing.T) {
	for _, component := range []string{"controller", "server"} {
		t.Run(component, func(t *testing.T) {
			class := controlPlanePriority(Config{Namespace: "custom-control"})
			for _, test := range []struct {
				name, priority, cpu, memory string
				changed                     bool
			}{
				{"operator", "operator-class", "250m", "512Mi", false},
				{"managed", class, "100m", "128Mi", true},
				{"mixed", class, "250m", "128Mi", true},
				{"other-install", "oberth-other-control-plane", "0.1", "134217728", false},
			} {
				t.Run(test.name, func(t *testing.T) {
					values := map[string]any{component: map[string]any{"priorityClassName": test.priority, "resources": map[string]any{"requests": map[string]any{"cpu": test.cpu, "memory": test.memory}, "limits": map[string]any{"memory": "2Gi"}}}}
					raw, _ := json.Marshal(values)
					deps := Deps{RunHelm: func(context.Context, []string) ([]byte, error) { return raw, nil }}
					path, changed, cleanup, err := dependencySchedulingValues(t.Context(), deps, "dependency", "namespace", component, class, true)
					defer cleanup()
					if err != nil || changed != test.changed {
						t.Fatal(changed, err)
					}
					if !changed {
						if path != "" {
							t.Fatal("operator values triggered a reset")
						}
						return
					}
					body, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var got map[string]any
					if err = json.Unmarshal(body, &got); err != nil {
						t.Fatal(err)
					}
					part := got[component].(map[string]any)
					if _, present := part["priorityClassName"]; present {
						t.Fatal("managed class retained")
					}
					resources := part["resources"].(map[string]any)
					if resources["limits"].(map[string]any)["memory"] != "2Gi" {
						t.Fatal("operator limit changed")
					}
					if test.name == "mixed" && resources["requests"].(map[string]any)["cpu"] != "250m" {
						t.Fatal("operator CPU request changed")
					}
				})
			}
		})
	}
}
