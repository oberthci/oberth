package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestArgoValuesHardPreflightRefusal(t *testing.T) {
	for _, mode := range []string{"plan", "apply", "json apply", "verify"} {
		t.Run(mode, func(t *testing.T) {
			marker := "private-argo-read-diagnostic"
			kube := fake.NewClientset()
			ca := newTestServerCA(t, []string{"api.example.test"})
			var output bytes.Buffer
			loads, reads := 0, 0
			host := InstallDeps{Output: &output, CredentialSink: func(string, string) {}, LookPath: func(name string) (string, error) { return name, nil },
				KubeContexts: func(string) ([]string, error) { return []string{"chosen"}, nil },
				LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
					loads++
					return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, "chosen", nil
				},
				RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
					t.Fatal("host command ran after failed Argo values read")
					return nil, nil
				},
				ProbeNetworkPolicy: func(context.Context, Config, Deps) NetworkPolicyProbeResult {
					t.Fatal("probe ran after failed Argo values read")
					return NetworkPolicyProbeResult{}
				},
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					if len(args) < 3 || args[0] != "--kube-context" || args[1] != "chosen" {
						t.Fatalf("Argo read lost selected context: %v", args)
					}
					args = args[2:]
					if args[0] == "list" {
						if args[slices.Index(args, "-n")+1] == DefaultArgoNamespace {
							return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
						}
						return []byte(`[{"name":"oberth"}]`), nil
					}
					if args[0] == "get" {
						if args[2] == "argo-workflows" {
							reads++
							return nil, errors.New(marker)
						}
						return []byte(`{"networkPolicy":{"enabled":true}}`), nil
					}
					t.Fatalf("mutation after failed Argo read: %v", args)
					return nil, nil
				},
			}
			cfg := Config{ContextName: "chosen", ClientAccess: "none"}
			var checks []CheckResult
			switch mode {
			case "plan":
				plan := PlanInstall(t.Context(), cfg, host)
				if plan.OK {
					t.Fatal("plan accepted unavailable Argo values")
				}
				checks = plan.Checks
			case "apply":
				err := Execute(t.Context(), cfg, host)
				var failure *installPreflightError
				if !errors.Is(err, errInstallPreflight) || !errors.As(err, &failure) {
					t.Fatal("apply lost typed Argo preflight failure", err)
				}
				checks = failure.checks
			case "json apply":
				if err := ExecuteJSON(t.Context(), cfg, host); err == nil {
					t.Fatal("JSON apply accepted unavailable Argo values")
				}
				var result InstallResult
				if err := json.Unmarshal(output.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if result.OK {
					t.Fatal("JSON apply reported success")
				}
				checks = result.Checks
			case "verify":
				result := VerifyInstall(t.Context(), cfg, host)
				if result.OK {
					t.Fatal("verify accepted unavailable Argo values")
				}
				checks = result.Checks
			}
			found := false
			for _, check := range checks {
				if check.Code == "argo_values" {
					found = true
					if check.OK || check.Warning || check.Remedy != argoValuesRemedy {
						t.Fatal("Argo values check was not a hard refusal", check)
					}
				}
			}
			if !found || loads != 1 || reads != 1 || len(kube.Actions()) != 0 || strings.Contains(output.String(), marker) {
				t.Fatalf("unsafe Argo-values refusal: found=%t loads=%d reads=%d API actions=%d", found, loads, reads, len(kube.Actions()))
			}
		})
	}
}
