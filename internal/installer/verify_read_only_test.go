package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestVerifyCreatesNoObjectsUnlessProbeRequested(t *testing.T) {
	for _, probe := range []bool{false, true} {
		name := "read-only"
		if probe {
			name = "live-probe"
		}
		t.Run(name, func(t *testing.T) {
			kube := fake.NewClientset(preflightNode(), preflightStorage(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace}})
			ca := newTestServerCA(t, []string{"api.example.test"})
			calls := 0
			var output bytes.Buffer
			host := InstallDeps{VerifyProbe: probe, Output: &output,
				LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
					return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, "selected", nil
				},
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					if strings.Contains(" "+strings.Join(args, " ")+" ", " list ") {
						return []byte(`[{"name":"oberth"}]`), nil
					}
					return []byte(`{"networkPolicy":{"enabled":true}}`), nil
				},
				RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
					command := strings.Join(args, " ")
					switch {
					case strings.Contains(command, "/proxy/configz"):
						return []byte(`{"kubeletconfig":{"systemReserved":{"cpu":"100m","memory":"128Mi"},"evictionHard":{"memory.available":"100Mi"}}}`), nil
					case strings.Contains(command, "stats/summary"):
						return preflightStats(ctx, input, name, args...)
					case strings.Contains(command, "admin read-file"):
						return ca, nil
					case strings.Contains(command, "admin readiness"):
						return []byte(`{"status":"not ready","code":"no_upstream"}`), nil
					case strings.Contains(command, "upstream list --check"):
						return []byte("NAME    KIND URL                         AUTH\nforge   ssh  ssh://git@forge.example/test ok\n"), nil
					default:
						t.Errorf("unexpected verification command: %s", command)
						return nil, nil
					}
				},
				ProbeNetworkPolicy: func(ctx context.Context, _ Config, _ Deps) NetworkPolicyProbeResult {
					calls++
					if _, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "probe"}}, metav1.CreateOptions{}); err != nil {
						return NetworkPolicyProbeResult{Status: "unverified"}
					}
					return NetworkPolicyProbeResult{Status: "enforced"}
				},
			}
			// The absent Deployment makes verification fail, but its JSON must
			// still be valid and contain no start/heartbeat prose from --probe.
			_ = PrintVerification(t.Context(), Config{}, host, true)
			var result InstallResult
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal("probe progress contaminated JSON", err)
			}
			mutations := 0
			for _, action := range kube.Actions() {
				if action.GetVerb() != "get" && action.GetVerb() != "list" {
					mutations++
				}
			}
			if !probe && (calls != 0 || mutations != 0) {
				t.Fatal("default verification mutated cluster", calls, mutations)
			}
			if probe && (calls != 1 || mutations != 1) {
				t.Fatal("explicit probe not invoked", calls, mutations)
			}
			found := false
			for _, check := range result.Checks {
				if check.Code != "network_policy" {
					continue
				}
				found = true
				if probe {
					if !check.OK || check.Reason != "enforced" {
						t.Fatal(check)
					}
				} else if !check.OK || check.Reason != "not-retested" || !strings.Contains(check.Evidence, "was measured at install; this verification did not re-test it") || !strings.Contains(check.Evidence, "--verify --probe") {
					t.Fatal("missing read-only explanation", check)
				}
			}
			if !found {
				t.Fatal("missing policy check", result.Checks)
			}
		})
	}
}
