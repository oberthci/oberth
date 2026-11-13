package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestRetiredGoProxyVerifyAndPlanKeepReadAccess(t *testing.T) {
	bootstrapCredentialInventory(t)
	for _, test := range []struct {
		name  string
		value any
	}{{"null", nil}, {"empty", map[string]any{}}, {"disabled", map[string]any{"enabled": false}}, {"enabled", map[string]any{"enabled": true}}} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"argo": map[string]any{"goProxy": test.value}, "networkPolicy": map[string]any{"enabled": true}})
			if err != nil {
				t.Fatal(err)
			}
			kube := fake.NewClientset(preflightNode(), preflightStorage())
			ca := newTestServerCA(t, []string{"api.example.test"})
			runHelm := func(_ context.Context, args []string) ([]byte, error) {
				joined := " " + strings.Join(args, " ") + " "
				if strings.Contains(joined, " list ") {
					return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed"}]`), nil
				}
				if strings.Contains(joined, " get values ") {
					return bytes.Clone(raw), nil
				}
				t.Fatalf("read-only inspection attempted Helm write: %v", args)
				return nil, nil
			}
			host := InstallDeps{Output: io.Discard, LookPath: func(name string) (string, error) { return name, nil }, LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
				return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, "chosen", nil
			}, RunHelm: runHelm, RunCommand: preflightStats, ProbeNetworkPolicy: enforcedPolicyFixture}
			cfg := Config{Namespace: "oberth", ContextName: "chosen"}
			// The shared read and installed-policy verification must remain usable,
			// including a later install write reusing this already-read snapshot.
			cached := cfg
			if err := readInstallValues(t.Context(), &cached, Deps{RunHelm: runHelm}); err != nil {
				t.Fatal("shared read refused installed values:", err)
			}
			if err := resolveInstalledPolicy(t.Context(), &cached, Deps{RunHelm: runHelm}); err != nil || cached.NetworkPolicy != "true" {
				t.Fatalf("installed policy lost: %s %v", cached.NetworkPolicy, err)
			}
			if err := resolveInstallRerunValues(t.Context(), &cached, Deps{RunHelm: runHelm}, ClusterInfo{}); err == nil || !strings.Contains(err.Error(), "Run oberth upgrade first") {
				t.Fatalf("cached install write did not refuse: %v", err)
			}
			result := VerifyInstall(t.Context(), cfg, host)
			network, upgrade := false, false
			for _, check := range result.Checks {
				if check.Code == "network_policy" {
					network = true
					if !check.OK || check.Reason == "unverified" {
						t.Fatal("verify lost installed policy", check)
					}
				}
				if check.Code == "installed_values" {
					upgrade = true
					if !check.OK || !check.Warning || !strings.Contains(check.Remedy, "Run oberth upgrade first") {
						t.Fatal("verify should report a non-blocking upgrade remedy", check)
					}
				}
			}
			if !network || !upgrade {
				t.Fatal("verify omitted installed policy or upgrade remedy", result.Checks)
			}
			plan := PlanInstall(t.Context(), cfg, host)
			found := false
			for _, check := range plan.Checks {
				if check.Code == "installed_values" {
					found = true
					if check.OK || !strings.Contains(check.Remedy, "argo.goProxy") || !strings.Contains(check.Remedy, "Run oberth upgrade first") || strings.Contains(check.Remedy, "Read the existing Helm") {
						t.Fatal("plan hid upgrade remedy", check)
					}
				}
			}
			if !found || plan.OK {
				t.Fatal("plan did not report required upgrade", plan.Checks)
			}
		})
	}
}
