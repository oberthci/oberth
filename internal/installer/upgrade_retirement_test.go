package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestUpgradeRefusesLegacyResourcesBeforeMutation(t *testing.T) {
	for _, scenario := range []string{"preflight", "apply preflight", "completed preflight", "deleting preflight", "vmi", "vm", "unlabelled vmi", "second page", "forbidden", "list missing", "malformed", "discovery unavailable", "clean", "no kubevirt"} {
		t.Run(scenario, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet {
					t.Errorf("preflight mutated API: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/apis/argoproj.io/v1alpha1" {
					if scenario == "discovery unavailable" {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					_, _ = io.WriteString(w, `{"groupVersion":"argoproj.io/v1alpha1","resources":[{"name":"workflows","kind":"Workflow","namespaced":true,"verbs":["list"]}]}`)
					return
				}
				if r.URL.Path == "/apis/kubevirt.io/v1" {
					if scenario == "no kubevirt" {
						http.Error(w, "not found", 404)
						return
					}
					_, _ = io.WriteString(w, `{"groupVersion":"kubevirt.io/v1","resources":[{"name":"virtualmachineinstances","kind":"VirtualMachineInstance","namespaced":true,"verbs":["list"]},{"name":"virtualmachines","kind":"VirtualMachine","namespaced":true,"verbs":["list"]}]}`)
					return
				}
				if scenario == "forbidden" {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				if scenario == "list missing" {
					http.Error(w, "not found", 404)
					return
				}
				if scenario == "malformed" {
					_, _ = io.WriteString(w, "not json")
					return
				}
				kind, namespace, annotations, labels := "Workflow", "custom-pipelines", map[string]string{}, map[string]string{}
				found, next := false, ""
				switch {
				case strings.HasSuffix(r.URL.Path, "/workflows"):
					if r.URL.Path != "/apis/argoproj.io/v1alpha1/namespaces/custom-pipelines/workflows" {
						t.Errorf("wrong pipeline scope: %s", r.URL.Path)
					}
					found = strings.Contains(scenario, "preflight")
					if scenario == "second page" {
						if r.URL.Query().Get("continue") == "page2" {
							found = true
						} else {
							next = "page2"
						}
					}
					annotations["oberth.ci/trusted-kvm-intent"] = "durable-intent"
				case strings.HasSuffix(r.URL.Path, "/virtualmachineinstances"):
					kind, namespace = "VirtualMachineInstance", "legacy-guests"
					found = scenario == "vmi" || scenario == "unlabelled vmi"
					if scenario == "unlabelled vmi" {
						annotations["oberth.ci/vm-spec"] = "durable-intent"
					} else {
						labels["oberth.ci/tier"] = "vm-runner"
					}
				case strings.HasSuffix(r.URL.Path, "/virtualmachines"):
					kind, namespace, found = "VirtualMachine", "legacy-guests", scenario == "vm"
					labels["oberth.ci/tier"] = "vm-runner"
				default:
					t.Errorf("unexpected API request %s", r.URL.Path)
				}
				items := []any{}
				if found {
					metadata := map[string]any{"name": "legacy-owned", "namespace": namespace, "annotations": annotations, "labels": labels}
					if scenario == "deleting preflight" {
						metadata["deletionTimestamp"] = "2026-11-03T12:00:00Z"
					}
					items = append(items, map[string]any{"apiVersion": "example.test/v1", "kind": kind, "metadata": metadata, "status": map[string]any{"phase": "Succeeded"}})
				} else {
					// Unrelated objects must not block an upgrade.
					items = append(items, map[string]any{"kind": kind, "metadata": map[string]any{"name": "unrelated", "namespace": namespace}})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "List", "metadata": map[string]any{"continue": next}, "items": items})
			}))
			defer server.Close()
			config := &rest.Config{Host: server.URL}
			kube, err := kubernetes.NewForConfig(config)
			if err != nil {
				t.Fatal(err)
			}
			mutations := 0
			deps := Deps{KubeClient: kube, RestConfig: config, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "list":
					return []byte(`[{"name":"oberth","chart":"oberth-0.17.80","status":"deployed","revision":1}]`), nil
				case "get":
					if args[1] == "values" {
						return []byte(`{"argo":{"namespace":"custom-pipelines"}}`), nil
					}
				case "history":
					return []byte(`[{"revision":1,"status":"deployed"}]`), nil
				default:
					mutations++
				}
				return nil, fmt.Errorf("unexpected Helm operation: %v", args)
			}, RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
				mutations++
				return nil, fmt.Errorf("unexpected command")
			}}
			_, err = RunUpgrade(t.Context(), UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.17.83", DryRun: scenario != "apply preflight"}, deps)
			if requests == 0 || mutations != 0 {
				t.Fatalf("requests=%d mutations=%d error=%v", requests, mutations, err)
			}
			switch scenario {
			case "clean", "no kubevirt":
				if err != nil {
					t.Fatal(err)
				}
			case "forbidden", "malformed", "list missing", "discovery unavailable":
				if err == nil || !strings.Contains(err.Error(), "inspect legacy") {
					t.Fatalf("read failure did not block retirement: %v", err)
				}
			default:
				if err == nil || !strings.Contains(err.Error(), "legacy-owned") || !strings.Contains(err.Error(), "upgrade refused") {
					t.Fatalf("legacy owner not named before upgrade: %v", err)
				}
				if scenario == "completed preflight" {
					for _, detail := range []string{"Completed preflight Workflows", "argo.workflowTTL", "default 1 hour", "current server"} {
						if !strings.Contains(err.Error(), detail) {
							t.Errorf("completed preflight refusal must explain %q: %v", detail, err)
						}
					}
				}
			}
		})
	}
}
