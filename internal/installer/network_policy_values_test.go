package installer

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"k8s.io/client-go/kubernetes/fake"
)

func TestStoredDisabledPolicySurvivesPreflightAndUpgrade(t *testing.T) {
	cfg := Config{Namespace: "oberth", NetworkPolicy: "auto"}
	var output bytes.Buffer
	deps := Deps{Output: &output, KubeClient: fake.NewClientset(preflightNode(), preflightStorage()), RunCommand: preflightStats, ProbeNetworkPolicy: enforcedPolicyFixture}
	result := InstallResult{OK: true}
	inspectCluster(t.Context(), &cfg, deps, &ClusterIdentity{}, &result, false, true)
	if !result.OK {
		t.Fatal(result.Checks)
	}
	deps.RunHelm = func(_ context.Context, args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "list" {
			return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.21.9"}]`), nil
		}
		if len(args) > 1 && args[0] == "get" && args[1] == "values" {
			return []byte(`{"networkPolicy":{"enabled":false}}`), nil
		}
		t.Fatalf("unexpected helm arguments %v", args)
		return nil, nil
	}
	if err := resolveInstallRerunValues(t.Context(), &cfg, deps, ClusterInfo{}); err != nil {
		t.Fatal(err)
	}
	if !cfg.existingRelease || cfg.NetworkPolicy != "auto" || valueAt(cfg.liveInstallValues, "networkPolicy", "enabled") != false {
		t.Fatalf("override not reproduced: %+v", cfg)
	}
	args := OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{})
	if !slices.Contains(args, "--reuse-values") || strings.Contains(strings.Join(args, " "), "networkPolicy.enabled=true") || !strings.Contains(output.String(), "WARNING: pipeline NetworkPolicy is disabled") || Review(cfg).NetworkPolicy != "false" {
		t.Fatal("stored choice or warning lost", args, output.String(), Review(cfg))
	}
	if err := resolveInstalledPolicy(t.Context(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	result = InstallResult{OK: true}
	inspectCluster(t.Context(), &cfg, deps, &ClusterIdentity{}, &result, false, true)
	if result.OK || result.Checks[len(result.Checks)-1].Reason != "disabled" {
		t.Fatal("verification reported flag default instead of installed false", result)
	}
}

func TestVerifyReportsStoredDisabledPolicyWithOmittedOrTrueFlag(t *testing.T) {
	for _, flag := range []string{"", "true"} {
		t.Run(flag, func(t *testing.T) {
			host := InstallDeps{Output: io.Discard, LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
				return fake.NewClientset(preflightNode(), preflightStorage()), &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: newTestServerCA(t, []string{"api.example.test"})}}, "chosen", nil
			},
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					if strings.Contains(" "+strings.Join(args, " ")+" ", " list ") {
						return []byte(`[{"name":"oberth"}]`), nil
					}
					return []byte(`{"networkPolicy":{"enabled":false}}`), nil
				}, RunCommand: preflightStats,
				ProbeNetworkPolicy: func(context.Context, Config, Deps) NetworkPolicyProbeResult {
					t.Fatal("disabled installed policy was presented as enforced capability")
					return NetworkPolicyProbeResult{}
				},
			}
			result := VerifyInstall(t.Context(), Config{NetworkPolicy: flag}, host)
			found := false
			for _, check := range result.Checks {
				if check.Code == "network_policy" {
					found = true
					if check.OK || check.Reason != "disabled" {
						t.Fatal("wrong effective installed setting", check)
					}
				}
			}
			if !found || result.OK {
				t.Fatal("missing disabled installed result", result)
			}
		})
	}
}
