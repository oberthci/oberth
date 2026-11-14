package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestExistingUpgradeDoesNotRequireFreshProbe(t *testing.T) {
	for _, stored := range []string{"true", "false", "missing"} {
		t.Run(stored, func(t *testing.T) {
			var output bytes.Buffer
			stopped := errors.New("review fixture stops before mutation")
			reads := 0
			probeCalls := 0
			kube := argoSecurityTestClient(preflightNode(), preflightStorage())
			host := testAdminInstallDeps(InstallDeps{Output: &output, CredentialSink: func(string, string) {},
				LookPath:     func(name string) (string, error) { return name, nil },
				KubeContexts: func(string) ([]string, error) { return []string{"chosen"}, nil },
				LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
					return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: newTestServerCA(t, []string{"api.example.test"})}}, "chosen", nil
				}, RunCommand: preflightStats,
				ProbeNetworkPolicy: func(context.Context, Config, Deps) NetworkPolicyProbeResult {
					if reads < 2 {
						t.Fatal("probe preceded stored values")
					}
					probeCalls++
					return NetworkPolicyProbeResult{Status: "unverified", Remedy: "fixture: scratch namespace forbidden"}
				},
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					reads++
					if strings.Contains(" "+strings.Join(args, " ")+" ", " list ") {
						return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.21.10"}]`), nil
					}
					if strings.Contains(" "+strings.Join(args, " ")+" ", " get values ") {
						if stored == "missing" {
							return []byte(`{}`), nil
						}
						return []byte(fmt.Sprintf(`{"networkPolicy":{"enabled":%s}}`, stored)), nil
					}
					return nil, stopped
				},
			})
			err := Execute(t.Context(), Config{ContextName: "chosen", Upgrade: true, NetworkPolicy: "auto", InstallSecretStoreDev: true, ChartPath: testLocalChart, ClientAccess: "none"}, host)
			t.Logf("stored=%s helmReads=%d preflightRejected=%v", stored, reads, errors.Is(err, errInstallPreflight))
			want := stored
			if stored == "missing" {
				want = "unspecified (preserved)"
			}
			if probeCalls != 1 || !strings.Contains(output.String(), "Pipeline NetworkPolicy: "+want) || !strings.Contains(output.String(), "capability: unverified") || (stored != "true" && !strings.Contains(output.String(), "WARNING:")) {
				t.Fatal(output.String(), probeCalls)
			}
			if reads == 0 || !errors.Is(err, stopped) || errors.Is(err, errInstallPreflight) {
				t.Fatalf("existing install --upgrade rejected before reading stored choice: %v", err)
			}
		})
	}
}

func TestRunUpgradePreservesPolicyWithoutProbePrivileges(t *testing.T) {
	for _, stored := range []string{"true", "false", "missing"} {
		t.Run(stored, func(t *testing.T) {
			var output bytes.Buffer
			image := "example.invalid/oberth@sha256:" + strings.Repeat("a", 64)
			replicas := int32(1)
			deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: "oberth", Generation: 1, Labels: map[string]string{"app.kubernetes.io/instance": "oberth"}}, Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "oberth", Image: image}}}}}, Status: appsv1.DeploymentStatus{ObservedGeneration: 1, UpdatedReplicas: 1, AvailableReplicas: 1}}
			applied := false
			values := map[string]any{}
			if stored != "missing" {
				values["networkPolicy"] = map[string]any{"enabled": stored == "true"}
			}
			raw, _ := json.Marshal(values)
			deps := testAdminDeps(Deps{Output: &output, ContextName: "chosen", KubeClient: argoSecurityTestClient(deployment), ProbeNetworkPolicy: func(context.Context, Config, Deps) NetworkPolicyProbeResult {
				t.Fatal("dedicated upgrade required fresh probe")
				return NetworkPolicyProbeResult{Status: "unverified"}
			},
				RunCommand: func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
					if strings.Contains(strings.Join(args, " "), "audit record") {
						return nil, nil
					}
					return []byte("oberth v0.21.11 commit=fixture\n"), nil
				},
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					if out, ok := testUpgradeArgoSecurityPreflight(args); ok {
						return out, nil
					}

					if args[0] == "get" && args[1] == "values" {
						if slices.Contains(args, "--all") {
							var compat map[string]any
							body, _ := testUpgradeValuePreflight(args)
							_ = json.Unmarshal(body, &compat)
							for k, v := range values {
								compat[k] = v
							}
							return json.Marshal(compat)
						}
						return raw, nil
					}
					if args[0] == "upgrade" {
						if !slices.Contains(args, "--reset-then-reuse-values") || strings.Contains(strings.Join(args, " "), "networkPolicy.enabled=") {
							t.Fatal("upgrade changed policy", args)
						}
						if slices.Contains(args, "--dry-run=server") {
							return []byte(`{"config":` + string(raw) + `}`), nil
						}
						applied = true
						return nil, nil
					}
					if out, ok := testUpgradeValuePreflight(args); ok {
						return out, nil
					}
					switch args[0] {
					case "list":
						return []byte(`[{"name":"oberth","status":"deployed","chart":"oberth-0.21.10"}]`), nil
					case "show":
						return []byte("image:\n  ref: " + image + "\n"), nil
					case "repo":
						return nil, nil
					}
					return nil, fmt.Errorf("unexpected helm %v", args)
				},
			})
			withUpgradeWorkflowAPI(t, &deps, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, upgradeWorkflowList())
			})
			result, err := RunUpgrade(t.Context(), UpgradeConfig{BinaryVersion: "0.21.11", Namespace: "oberth", Yes: true, NoBackup: true, ChartOverride: testLocalChart, Timeout: time.Second}, deps)
			if err != nil || !result.Upgraded || !applied {
				t.Fatal("maintenance refused", err, output.String())
			}
			want := stored
			if stored == "missing" {
				want = "unspecified (preserved)"
			}
			if !strings.Contains(output.String(), "Pipeline NetworkPolicy: "+want) || !strings.Contains(output.String(), "capability: unverified") || (stored != "true" && !strings.Contains(output.String(), "WARNING:")) {
				t.Fatal(output.String())
			}
		})
	}
}
