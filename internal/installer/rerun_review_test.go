package installer

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

func TestReleaseLookupFailureCannotInstall(t *testing.T) {
	for _, mode := range []string{"transport", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			writes := 0
			deps := Deps{Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				if args[0] == "list" {
					if mode == "transport" {
						return nil, errors.New("temporary helm failure")
					}
					return []byte("not json"), nil
				}
				if args[0] == "upgrade" {
					writes++
				}
				return nil, nil
			}}
			_, err := InstallOberth(t.Context(), Config{ChartPath: "local-chart"}, deps, OpenBaoResult{}, RekorResult{})
			if err == nil || writes != 0 {
				t.Fatalf("uncertain release state caused apply: err=%v writes=%d", err, writes)
			}
		})
	}
}
func TestReleaseLookupIncludesPendingStates(t *testing.T) {
	deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		if !slices.Contains(args, "-a") && !slices.Contains(args, "--all") {
			t.Error("release lookup excludes pending states")
		}
		return []byte(`[]`), nil
	}}
	findHelmRelease(t.Context(), deps, "oberth", "oberth")
}
func TestRerunArgumentsKeepOperatorValues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cfg       Config
		key, want string
	}{
		{name: "grant lifetime", cfg: Config{}, key: "promotion.grantMaxLifetime=", want: ""},
		{name: "pending witness", cfg: Config{InstallRekor: true}, key: "auditAnchor.acceptWitnessGenesis=", want: ""},
		{name: "explicit role", cfg: Config{InstallSecretStore: true, ArgoVaultCredentialedRole: "custom-role"}, key: "argo.vault.credentialedRole=", want: "argo.vault.credentialedRole=custom-role"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found := ""
			count := 0
			for _, arg := range OberthHelmArgs(tc.cfg, OpenBaoResult{}, RekorResult{}) {
				if strings.HasPrefix(arg, tc.key) {
					found = arg
					count++
				}
			}
			if found != tc.want || count > 1 {
				t.Fatalf("last override=%q count=%d want=%q", found, count, tc.want)
			}
		})
	}
	_, args := upgradeHelmArgs(UpgradeConfig{}, "chart", "image", backupCompatibility{})
	for _, arg := range args {
		if strings.HasPrefix(arg, "promotion.grantMaxLifetime=") {
			t.Fatal("upgrade overwrites administrator grant lifetime")
		}
	}
}

func TestRerunResolutionPreservesLiveDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, network, banner   string
		wantNetwork, wantBanner string
	}{
		{"no flags", "auto", "", "auto", ""},
		{"explicit settings", "true", "https://new.example/runs", "true", "https://new.example/runs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Namespace: "oberth", NetworkPolicy: tc.network, PushBannerURL: tc.banner}
			deps := Deps{Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				if args[0] == "list" {
					return []byte(`[{"name":"oberth","status":"deployed"}]`), nil
				}
				return []byte(`{"networkPolicy":{"enabled":false},"pushBannerURL":"https://old.example/runs","argo":{"vault":{"credentialedRole":"custom-live"}}}`), nil
			}}
			if err := resolveInstallRerunValues(t.Context(), &cfg, deps, ClusterInfo{Engine: "k3s"}); err != nil {
				t.Fatal(err)
			}
			if cfg.NetworkPolicy != tc.wantNetwork || cfg.PushBannerURL != tc.wantBanner {
				t.Fatalf("resolved=%+v", cfg)
			}
			args := strings.Join(OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{}), " ")
			if tc.banner == "" && strings.Contains(args, "pushBannerURL=") {
				t.Fatal("implicit banner override")
			}
		})
	}
}
func TestUnknownClusterCannotAutoChooseNetworkPolicy(t *testing.T) {
	cfg := Config{NetworkPolicy: "auto"}
	if err := resolveInstallRerunValues(t.Context(), &cfg, Deps{}, ClusterInfo{Engine: "unknown", NodesUnavailable: true}); err == nil {
		t.Fatal("unknown CNI silently enables network policy")
	}
}
func TestIdentityProducerFailureCannotFallBackToShared(t *testing.T) {
	live := map[string]any{"argo": map[string]any{"perRepoIdentities": []any{"existing"}}}
	for _, err := range []error{errors.New("producer unavailable"), nil} {
		if checkProducedIdentities(live, "perRepoIdentities", 0, err) == nil {
			t.Fatal("shared fallback allowed with live per-repo identities")
		}
	}
}
func TestValuesPreviewListsAllChangesWithoutValues(t *testing.T) {
	var out strings.Builder
	deps := Deps{Output: &out, RunHelm: func(context.Context, []string) ([]byte, error) {
		return []byte(`{"a":{"same":1,"changed":"old-secret","removed":true},"list":["a"]}`), nil
	}}
	raw := []byte(`{"config":{"a":{"same":1,"changed":"new-secret","added":false},"list":["b"]},"chart":{"values":{}}}`)
	if err := printHelmValueChanges(t.Context(), deps, "oberth", raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"a.changed", "a.removed", "a.added", "list"} {
		if !strings.Contains(out.String(), key) {
			t.Fatalf("missing %s: %s", key, &out)
		}
	}
	for _, secret := range []string{"old-secret", "new-secret", "a.same"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("preview leaks unchanged key or value: %s", &out)
		}
	}
	if err := printHelmValueChanges(t.Context(), deps, "oberth", []byte(`{"name":"oberth"}`)); err == nil {
		t.Fatal("missing preview configuration accepted")
	}
}

func TestSameVersionExplicitInstallValuesAreApplied(t *testing.T) {
	writes := 0
	deps := Deps{Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte(`[{"name":"oberth","chart":"oberth-0.17.62","status":"deployed"}]`), nil
		case "show":
			return []byte("image:\n  ref: example.invalid/oberth@sha256:" + strings.Repeat("1", 64) + "\n"), nil
		case "upgrade":
			writes++
		}
		return nil, nil
	}}
	_, err := InstallOberth(t.Context(), Config{ChartVersion: "v0.17.62", PushBannerURL: "https://new.example/runs", ImageRef: "example.invalid/oberth@sha256:" + strings.Repeat("1", 64)}, deps, OpenBaoResult{}, RekorResult{})
	if err != nil || writes != 1 {
		t.Fatalf("explicit same-version change was skipped: %v writes=%d", err, writes)
	}
}

// Reused values supply every unmentioned key. Only an explicit option may
// append a replacement after them; check each setting independently.
func TestRerunEachLiveSettingRequiresAnExplicitOverride(t *testing.T) {
	for _, tc := range []struct {
		key, live, replacement string
		configure              func(*Config, bool)
	}{
		{"networkPolicy.enabled", "false", "true", func(c *Config, explicit bool) {
			c.NetworkPolicy = "auto"
			if explicit {
				c.NetworkPolicy = "true"
			}
		}},
		{"service.type", "LoadBalancer", "ClusterIP", func(c *Config, explicit bool) {
			if explicit {
				c.ServiceType = "ClusterIP"
			}
		}},
		{"pushBannerURL", "https://old.example/runs", "https://new.example/runs", func(c *Config, explicit bool) {
			c.TLSExtraDNSNames = []string{"detected.example"}
			if explicit {
				c.PushBannerURL = "https://new.example/runs"
				c.PushBannerURLSet = true
			}
		}},
		{"argo.vault.credentialedRole", "live-role", "new-role", func(c *Config, explicit bool) {
			c.InstallSecretStore = true
			if explicit {
				c.ArgoVaultCredentialedRole = "new-role"
			}
		}},
		{"auditAnchor.acceptWitnessGenesis", "1:pending", "2:replacement", func(c *Config, explicit bool) {
			c.InstallRekor = true
			if explicit {
				c.AcceptWitnessGenesis = "2:replacement"
				c.AcceptWitnessGenesisSet = true
			}
		}},
	} {
		t.Run(tc.key, func(t *testing.T) {
			for _, explicit := range []bool{false, true} {
				cfg := Config{existingRelease: true, liveInstallValues: map[string]any{"argo": map[string]any{"vault": map[string]any{"credentialedRole": "live-role"}}}}
				tc.configure(&cfg, explicit)
				args := OberthHelmArgs(cfg, OpenBaoResult{}, RekorResult{})
				if !slices.Contains(args, "--reuse-values") {
					t.Fatal("live values would not be reused")
				}
				effective := tc.live
				count := 0
				for _, arg := range args {
					if strings.HasPrefix(arg, tc.key+"=") {
						effective = strings.TrimPrefix(arg, tc.key+"=")
						count++
					}
				}
				want := tc.live
				if explicit {
					want = tc.replacement
				}
				if effective != want || count > 1 || (!explicit && count != 0) {
					t.Fatalf("explicit=%v effective=%q overrides=%d want=%q", explicit, effective, count, want)
				}
			}
		})
	}
	for _, explicit := range []bool{false, true} {
		cfg := UpgradeConfig{}
		if explicit {
			cfg.UserSet = []string{"promotion.grantMaxLifetime=12h"}
		}
		_, args := upgradeHelmArgs(cfg, "chart", "image", backupCompatibility{})
		effective := "36h"
		for _, arg := range args {
			if strings.HasPrefix(arg, "promotion.grantMaxLifetime=") {
				effective = strings.TrimPrefix(arg, "promotion.grantMaxLifetime=")
			}
		}
		want := "36h"
		if explicit {
			want = "12h"
		}
		if effective != want {
			t.Fatalf("grant lifetime explicit=%v got=%q want=%q", explicit, effective, want)
		}
	}
}
