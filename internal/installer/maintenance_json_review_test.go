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

func TestMaintenanceJSONPreservesFailedProbeMeasurement(t *testing.T) {
	for _, status := range []string{"absent", "unverified"} {
		t.Run(status, func(t *testing.T) {
			kube := fake.NewClientset(preflightNode(), preflightStorage(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "oberth", Namespace: DefaultNamespace}})
			ca := newTestServerCA(t, []string{"api.example.test"})
			config := &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}
			var output bytes.Buffer
			calls := 0
			measured := NetworkPolicyProbeResult{Status: status, Evidence: "Measured fixture " + status, Remedy: "Repair enforcement and request a live probe."}
			host := InstallDeps{Output: &output, LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) { return kube, config, "chosen", nil }, RunCommand: preflightStats,
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					if strings.Contains(" "+strings.Join(args, " ")+" ", " list ") {
						return []byte(`[{"name":"oberth"}]`), nil
					}
					return []byte(`{"networkPolicy":{"enabled":true}}`), nil
				}, ProbeNetworkPolicy: func(context.Context, Config, Deps) NetworkPolicyProbeResult { calls++; return measured },
			}
			_ = executeJSON(t.Context(), Config{ContextName: "chosen"}, host, func(ctx context.Context, cfg Config, deps InstallDeps) error {
				cfg.existingRelease = true
				preflight := InstallResult{OK: true}
				inspectCluster(ctx, &cfg, Deps{KubeClient: kube, RestConfig: config, ContextName: "chosen", RunCommand: preflightStats, ProbeNetworkPolicy: deps.ProbeNetworkPolicy}, &ClusterIdentity{}, &preflight, true, true)
				if !preflight.OK {
					t.Fatal("maintenance informational behavior changed", preflight.Checks)
				}
				deps.installResolved(cfg)
				return nil
			})
			var result InstallResult
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, check := range result.Checks {
				if check.Code != "network_policy" {
					continue
				}
				found = true
				if check.OK || !check.Warning || check.Reason != status || check.Evidence != measured.Evidence || check.Remedy != measured.Remedy {
					t.Fatal("measured failure rewritten", check)
				}
			}
			if !found || calls != 1 || strings.Contains(output.String(), "proven") || strings.Contains(output.String(), "not-retested") {
				t.Fatal("false measurement claim or duplicate probe", calls, output.String())
			}
		})
	}
}
