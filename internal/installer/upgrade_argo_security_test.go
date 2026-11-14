package installer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestUpgradeArgoSecurityBeforePolicy(t *testing.T) {
	for _, mode := range []string{"managed", "operator-managed", "skip-argo", "unreadable", "unsafe-explicit", "native-fails", "dry-run", "external-dry-run", "old-profile", "no-consent", "live-compliant", "external-compliant", "post-reconcile-stale", "old-profile-compliant"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newUpgradeFixture(t, "0.16.20", "0.16.21", "73")
			setArgoSecurityLive(t, fixture.deps.KubeClient, DefaultArgoNamespace, strings.Contains(mode, "compliant"))
			original := fixture.deps.RunHelm
			nativeWrites, policyWrites := 0, 0
			var nativeArgs []string
			stored := `{"mainContainer":{"securityContext":{"runAsUser":0,"readOnlyRootFilesystem":false}},"executor":{"resources":{"requests":{"cpu":"75m"}}},"controller":{"parallelism":9,"namespaceParallelism":5,"config":{"archiveLogs":true}},"server":{"enabled":true}}`
			if strings.HasPrefix(mode, "old-profile") {
				stored = `{"controller":{"configMap":{"create":false}}}`
			}
			if mode == "unsafe-explicit" {
				stored = `{"mainContainer":{"securityContext":{"allowPrivilegeEscalation":true}}}`
			}
			fixture.deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == "list" && slices.Contains(args, DefaultArgoNamespace) {
					if mode == "operator-managed" || mode == "skip-argo" || mode == "external-dry-run" || mode == "external-compliant" {
						return []byte(`[]`), nil
					}
					return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
				}
				if len(args) > 2 && args[0] == "get" && args[1] == "values" && args[2] == "argo-workflows" {
					if mode == "unreadable" {
						return nil, errors.New("operator access denied")
					}
					return []byte(stored), nil
				}
				if args[0] == "repo" {
					return nil, nil
				}
				if len(args) > 1 && args[0] == "upgrade" && !slices.Contains(args, "--dry-run=server") {
					if args[1] == "argo-workflows" {
						nativeWrites++
						nativeArgs = slices.Clone(args)
						if mode == "native-fails" {
							return nil, errors.New("native Helm failure")
						}
						if mode != "post-reconcile-stale" {
							setArgoSecurityLive(t, fixture.deps.KubeClient, DefaultArgoNamespace, true)
						}
						return nil, nil
					}
					policyWrites++
					if nativeWrites != 1 && !strings.Contains(mode, "compliant") {
						t.Fatal("Oberth policy deployed before native Argo defaults")
					}
				}
				return original(ctx, args)
			}
			cfg := UpgradeConfig{ArgoSecurityDefaults: mode != "no-consent", Namespace: "oberth", BinaryVersion: "0.16.21", BinarySchemaVersion: 73, NoBackup: true, DryRun: mode == "dry-run" || mode == "external-dry-run"}
			result, err := RunUpgrade(t.Context(), cfg, fixture.deps)
			switch mode {
			case "managed":
				if err != nil || !result.Upgraded || nativeWrites != 1 || policyWrites != 1 {
					t.Fatal(result, nativeWrites, policyWrites, err)
				}
				if !slices.Contains(nativeArgs, "--reuse-values") || slices.Contains(nativeArgs, "--reset-values") || slices.Contains(nativeArgs, "--reset-then-reuse-values") || slices.Contains(nativeArgs, "server.enabled=false") {
					t.Fatal("operator Argo values overridden", nativeArgs)
				}
				for _, arg := range nativeArgs {
					if strings.HasPrefix(arg, "mainContainer.securityContext.runAsUser=") || strings.HasPrefix(arg, "mainContainer.securityContext.readOnlyRootFilesystem=") || strings.HasPrefix(arg, "controller.parallelism=") || strings.HasPrefix(arg, "controller.namespaceParallelism=") {
						t.Fatal("operator value overwritten", arg)
					}
				}
				if !slices.Contains(nativeArgs, `controller.podSecurityContext.seccompProfile.type="RuntimeDefault"`) {
					t.Fatal("controller default absent", nativeArgs)
				}
			case "live-compliant", "external-compliant", "old-profile-compliant":
				if err != nil || !result.Upgraded || nativeWrites != 0 || policyWrites != 1 {
					t.Fatal(result, nativeWrites, policyWrites, err)
				}
			case "old-profile":
				if err == nil || nativeWrites != 0 || policyWrites != 0 || !strings.Contains(err.Error(), "v0.22.5 migration") {
					t.Fatal(nativeWrites, policyWrites, err)
				}
			case "dry-run":
				if err != nil || nativeWrites != 0 || policyWrites != 0 {
					t.Fatal(nativeWrites, policyWrites, err)
				}
			default:
				if err == nil || policyWrites != 0 || (mode != "native-fails" && mode != "post-reconcile-stale" && nativeWrites != 0) {
					t.Fatal("unsafe upgrade was not refused in preflight", nativeWrites, policyWrites, err)
				}
				paths := []string{"mainContainer.securityContext.allowPrivilegeEscalation=false", `executor.securityContext.capabilities.drop=["ALL"]`}
				if mode != "operator-managed" && mode != "skip-argo" && mode != "external-dry-run" {
					paths = append(paths, `controller.podSecurityContext.seccompProfile.type="RuntimeDefault"`, `server.podSecurityContext.seccompProfile.type="RuntimeDefault"`)
				}
				for _, path := range paths {
					if !strings.Contains(err.Error(), path) {
						t.Fatalf("exact operator remedy %s missing: %v", path, err)
					}
				}
			}
			if mode == "managed" || mode == "dry-run" {
				if !strings.Contains(fixture.output.String(), "Argo controller.podSecurityContext:") || !strings.Contains(fixture.output.String(), "Argo executor.securityContext:") {
					t.Fatal("upgrade review omitted native security changes", fixture.output.String())
				}
			}
		})
	}
}

func TestUpgradeArgoSecurityUsesPlannedNamespaceAndRechecks(t *testing.T) {
	stored := `{}`
	readNamespaces := []string{}
	writes := 0
	deps := Deps{KubeClient: argoSecurityTestClient(argoSecurityLiveObjects("operator-pipelines", false)...), RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "list" {
			i := slices.Index(args, "-n")
			readNamespaces = append(readNamespaces, args[i+1])
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
		}
		if args[0] == "get" {
			return []byte(stored), nil
		}
		writes++
		return nil, errors.New("unexpected write")
	}}
	plan, err := planUpgradeArgoSecurityPreview(t.Context(), deps, []byte(`{"chart":{"values":{"argo":{"namespace":"old"}}},"config":{"argo":{"namespace":"operator-pipelines"}}}`), true)
	if err != nil || plan == nil || !slices.Equal(readNamespaces, []string{"operator-pipelines"}) {
		t.Fatal(plan, readNamespaces, err)
	}
	// A later operator override must stop the apply before either release changes.
	stored = `{"executor":{"securityContext":{"capabilities":{"drop":[]}}}}`
	if err := plan.reconcile(t.Context(), deps); err == nil || !strings.Contains(err.Error(), "operator-set executor.securityContext") || writes != 0 {
		t.Fatalf("unsafe concurrent override was not refused before writes: writes=%d err=%v", writes, err)
	}
}
