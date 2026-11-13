package installer

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestArgoPlanReviewUsesNativeValues(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, explicit := range []bool{false, true} {
			cfg := Config{ContextName: "chosen"}
			if explicit {
				global, namespace := 8, 2
				cfg.ArgoParallelism, cfg.ArgoNamespaceParallelism = &global, &namespace
			}
			ca := newTestServerCA(t, []string{"api.example.test"})
			kube := argoSecurityTestClient(preflightNode(), preflightStorage())
			reads, resourceReads := 0, 0
			host := InstallDeps{Output: io.Discard, LookPath: func(name string) (string, error) { return name, nil },
				LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
					return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, "chosen", nil
				},
				RunCommand: preflightStats, ProbeNetworkPolicy: enforcedPolicyFixture,
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					if len(args) < 3 || args[0] != "--kube-context" || args[1] != "chosen" {
						t.Fatalf("review lost context: %v", args)
					}
					args = args[2:]
					if args[0] == "list" {
						ns := args[slices.Index(args, "-n")+1]
						if existing && ns == DefaultArgoNamespace {
							return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
						}
						return []byte(`[]`), nil
					}
					if args[0] == "get" && args[2] == "argo-workflows" {
						if slices.Contains(args, "--all") {
							reads++
							return []byte(`{"controller":{"parallelism":17,"namespaceParallelism":5}}`), nil
						}
						resourceReads++
						return []byte(`{"executor":{"resources":{"requests":{"cpu":"75m"}}}}`), nil
					}
					t.Fatalf("unexpected review command: %v", args)
					return nil, nil
				},
			}
			plan := PlanInstall(t.Context(), cfg, host)
			if !plan.OK {
				t.Fatalf("plan failed: %+v", plan.Checks)
			}
			global, namespace := "4 (fresh default)", "0 (fresh default)"
			if existing {
				global, namespace = "17 (preserved)", "5 (preserved)"
			}
			if explicit {
				global, namespace = "8 (requested)", "2 (requested)"
			}
			if plan.Review.ArgoParallelism != global || plan.Review.ArgoNamespaceParallelism != namespace {
				t.Fatalf("review changed native values: %+v", plan.Review)
			}
			if existing && (reads != 1 || resourceReads != 2) {
				t.Fatal("effective limits read once; raw resources and security each read once", reads, resourceReads)
			}
		}
	}
}

func TestArgoUpgradePreservesOperatorLimits(t *testing.T) {
	raw := []byte(`{"controller":{"parallelism":17,"namespaceParallelism":5}}`)
	applied := false
	deps := Deps{KubeClient: argoSecurityTestClient(), Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
		case "get":
			return raw, nil
		case "upgrade":
			applied = true
			if !slices.Contains(args, "--reuse-values") || slices.Contains(args, "--reset-values") || slices.Contains(args, "--reset-then-reuse-values") || slices.Contains(args, "--values") {
				t.Fatal("native upgrade did not preserve stored values directly", args)
			}
			for _, key := range []string{"controller.parallelism=", "controller.namespaceParallelism="} {
				if strings.Contains(strings.Join(args, " "), key) {
					t.Fatal("upgrade set an unrequested native limit", args)
				}
			}
		}
		return nil, nil
	}}
	cfg := Config{Upgrade: true}
	if err := readArgoReviewValues(t.Context(), &cfg, deps); err != nil {
		t.Fatal(err)
	}
	review := Review(cfg)
	if review.ArgoParallelism != "17 (preserved)" || review.ArgoNamespaceParallelism != "5 (preserved)" || argoLimitsRequested(cfg) {
		t.Fatal("stored values became requested overrides", review)
	}
	if _, err := InstallArgoWorkflows(t.Context(), cfg, deps); err != nil || !applied {
		t.Fatal("upgrade did not reconcile", err)
	}
}
