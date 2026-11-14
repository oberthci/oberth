package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

func TestUpgradePinsTLSOverridesOnlyForLivePKI(t *testing.T) {
	t.Setenv(baoTokenEnvVar, "")
	for _, enabled := range []bool{false, true} {
		for _, dryRun := range []bool{false, true} {
			for _, override := range []struct{ set, values string }{
				{set: "tls.pki.enabled=false"},
				{set: "tls.pki.mount=other"},
				{set: "tls.pki=null"},
				{set: "tls=null"},
				{values: "tls:\n  extraDNSNames: [ci.example.com]\n"},
				{values: "tls:\n  extraIPs: [192.0.2.1]\n"},
				{values: "tls:\n  pki:\n    enabled: false\n"},
				{values: "tls: null\n"},
			} {
				t.Run(fmt.Sprintf("live-pki=%t/dry-run=%t/%s%s", enabled, dryRun, override.set, override.values), func(t *testing.T) {
					cfg := UpgradeConfig{BinaryVersion: "0.17.81", ChartOverride: "fixture", DryRun: dryRun, NoBackup: true}
					if override.set != "" {
						cfg.UserSet = []string{override.set}
					} else {
						path := filepath.Join(t.TempDir(), "values.yaml")
						if err := os.WriteFile(path, []byte(override.values), 0o600); err != nil {
							t.Fatal(err)
						}
						cfg.UserValues = []string{path}
					}
					applied := false
					stop := errors.New("stop after Helm apply")
					deps := testAdminDeps(Deps{Output: io.Discard, KubeClient: argoSecurityTestClient(), RestConfig: &rest.Config{Host: "https://127.0.0.1:6443"},
						RunHelm: func(_ context.Context, args []string) ([]byte, error) {
							if out, ok := testUpgradeArgoSecurityPreflight(args); ok {
								return out, nil
							}

							switch args[0] {
							case "list":
								// A failed latest revision must not select the TLS pin policy.
								return []byte(`[{"name":"oberth","status":"failed","chart":"oberth-0.17.80"}]`), nil
							case "history":
								return []byte(`[{"revision":3,"status":"failed"},{"revision":2,"status":"deployed"}]`), nil
							case "get":
								if !slices.Contains(args, "--revision") || !slices.Contains(args, "2") {
									t.Fatalf("TLS policy must use the deployed revision: %v", args)
								}
								if !slices.Contains(args, "--all") {
									return fmt.Appendf(nil, `{"tls":{"pki":{"enabled":%t}}}`, enabled), nil
								}
							case "show":
								return []byte("image:\n  ref: example.invalid/oberth@sha256:" + strings.Repeat("1", 64) + "\ntls:\n  pki:\n    enabled: false\n    mount: oberth-pki\n"), nil
							case "template":
								if enabled {
									t.Fatal("TLS override reached chart rendering on a live PKI release")
								}
								return nil, nil
							case "upgrade":
								if slices.Contains(args, "--dry-run=server") {
									return []byte(`{"config":{}}`), nil
								}
								applied = true
								return nil, stop
							}
							if out, ok := testUpgradeValuePreflight(args); ok {
								return out, nil
							}
							return nil, fmt.Errorf("unexpected helm call: %v", args)
						},
						RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
							t.Fatal("override validation must not inspect or write PKI")
							return nil, nil
						},
					})
					_, err := RunUpgrade(t.Context(), cfg, deps)
					if enabled {
						var pinned *ErrPinnedKeyOverride
						if !errors.As(err, &pinned) || applied || !strings.Contains(err.Error(), "PKI is enabled on the live release") || !strings.Contains(err.Error(), "--tls-extra-") {
							t.Fatalf("PKI override refusal lacks flag guidance: applied=%t err=%v", applied, err)
						}
					} else if (dryRun && (err != nil || applied)) || (!dryRun && (!errors.Is(err, stop) || !applied)) {
						t.Fatalf("non-PKI TLS override refused: applied=%t err=%v", applied, err)
					}
				})
			}
		}
	}
}
