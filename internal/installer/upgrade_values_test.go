package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestUpgradeRequiresSupportedHelm(t *testing.T) {
	for _, version := range []string{"v3.13.3", "v3.14.0-rc.1", "v3.14.0", "v3.18.6+gfixture", "v4.2.3", "", "unparseable"} {
		t.Run(version, func(t *testing.T) {
			calls := 0
			deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				calls++
				if !slices.Equal(args, []string{"version", "--template", "{{.Version}}"}) {
					t.Fatal(args)
				}
				return []byte(version), nil
			}}
			err := requireUpgradeHelmVersion(t.Context(), deps)
			supported := version == "v3.14.0" || version == "v3.18.6+gfixture" || version == "v4.2.3"
			if (err == nil) != supported || calls != 1 {
				t.Fatalf("version=%s err=%v calls=%d", version, err, calls)
			}
			if err != nil && !strings.Contains(err.Error(), "Helm >= 3.14.0") {
				t.Fatal(err)
			}
		})
	}
	want := errors.New("Helm unavailable")
	if err := requireUpgradeHelmVersion(t.Context(), Deps{RunHelm: func(context.Context, []string) ([]byte, error) { return nil, want }}); !errors.Is(err, want) {
		t.Fatal(err)
	}
}

func TestUpgradeDeployedRevision(t *testing.T) {
	for _, test := range []struct {
		name, history, want string
	}{
		{"highest-deployed-not-latest", `[{"revision":5,"status":"failed"},{"revision":4,"status":"deployed"},{"revision":2,"status":"deployed"}]`, "4"},
		{"invalid-json", `invalid`, ""},
		{"no-deployed", `[{"revision":5,"status":"failed"}]`, ""},
		{"invalid-revision", `[{"revision":0,"status":"deployed"}]`, ""},
		{"empty", `[]`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				if !slices.Equal(args, []string{"history", "oberth", "-n", "chosen", "-o", "json"}) {
					t.Fatal(args)
				}
				return []byte(test.history), nil
			}}
			got, err := deployedUpgradeRevision(t.Context(), "chosen", deps)
			if got != test.want || (err == nil) != (test.want != "") {
				t.Fatalf("revision = %q, err = %v; want %q", got, err, test.want)
			}
		})
	}
	failure := errors.New("history unavailable")
	_, err := deployedUpgradeRevision(t.Context(), "chosen", Deps{RunHelm: func(context.Context, []string) ([]byte, error) { return nil, failure }})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestUpgradeCompatibilityNeverFallsBackToNewDefaults(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `invalid`, `{"compatibility":null}`, `{"compatibility":{"schemaIdentity":"old","auditDomain":"old"}}`, `{"compatibility":{"schemaIdentity":5,"auditDomain":"old","witnessKeyInfo":"old"}}`, `{"compatibility":{"schemaIdentity":"old","auditDomain":"bad\nvalue","witnessKeyInfo":"old"}}`} {
		deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			if !slices.Contains(args, "--all") {
				t.Fatal("identities must include old defaults")
			}
			return []byte(raw), nil
		}}
		if _, err := readUpgradeCompatibility(t.Context(), "chosen", "1", deps); err == nil {
			t.Fatalf("accepted %s", raw)
		} else if raw == `{}` && (!strings.Contains(err.Error(), "older than v0.17.5") || !strings.Contains(err.Error(), "existing database and witness configuration")) {
			t.Fatalf("missing legacy-release recovery hint: %v", err)
		}
	}
	failure := errors.New("cannot read current release")
	_, err := readUpgradeCompatibility(t.Context(), "chosen", "1", Deps{RunHelm: func(context.Context, []string) ([]byte, error) { return nil, failure }})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestUpgradeCompatibilityOverridesArePinned(t *testing.T) {
	for _, key := range []string{"compatibility", "compatibility.schemaIdentity", "compatibility.auditDomain", "compatibility.witnessKeyInfo"} {
		if err := checkPinnedKeyConflicts([]string{key + "=foreign"}, nil); err == nil {
			t.Fatalf("identity override accepted: %s", key)
		}
	}
	for _, body := range []string{"compatibility: null\n", "compatibility:\n  schemaIdentity: foreign\n", "compatibility:\n  auditDomain: foreign\n", "compatibility:\n  witnessKeyInfo: foreign\n"} {
		path := filepath.Join(t.TempDir(), "values.yaml")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := checkPinnedKeyConflicts(nil, []string{path}); err == nil {
			t.Fatalf("identity values override accepted: %s", body)
		}
	}
}

func TestUpgradeSchemaPreflightUsesOnlyStoredOverridesAndCleansFiles(t *testing.T) {
	for _, failAt := range []string{"", "stored-read", "stored-json", "schema"} {
		t.Run(failAt, func(t *testing.T) {
			calls := []string{}
			temporary := ""
			deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				calls = append(calls, args[0])
				if args[0] == "history" {
					out, _ := testUpgradeValuePreflight(args)
					return out, nil
				}
				if args[0] == "get" {
					if slices.Contains(args, "--all") {
						raw, _ := testUpgradeValuePreflight(args)
						return raw, nil
					}
					if failAt == "stored-read" {
						return nil, errors.New("read failed")
					}
					if failAt == "stored-json" {
						return []byte("invalid"), nil
					}
					return []byte(`{"pushBannerURL":"","retention":{"keepNewestPerRef":0},"podLabels":{"arbitrary.example/key":"kept"}}`), nil
				}
				if args[0] != "template" {
					t.Fatalf("preflight mutated release: %v", args)
				}
				if slices.Contains(args, "--wait") || slices.Contains(args, "--timeout") || slices.Contains(args, "--reset-then-reuse-values") {
					t.Fatal(args)
				}
				if !slices.Contains(args, "--is-upgrade") || !slices.Contains(args, "compatibility.schemaIdentity="+testUpgradeCompatibility.SchemaIdentity) {
					t.Fatal(args)
				}
				temporary = args[slices.Index(args, "--values")+1]
				info, err := os.Stat(temporary)
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("unsafe snapshot: %v %v", info, err)
				}
				body, err := os.ReadFile(temporary)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(body), "compatibility") || !strings.Contains(string(body), "keepNewestPerRef") {
					t.Fatalf("snapshot must contain user overrides, not old defaults: %s", body)
				}
				if failAt == "schema" {
					return nil, errors.New("additional properties removedKey not allowed")
				}
				return nil, nil
			}}
			prepared, err := prepareUpgradeValues(t.Context(), UpgradeConfig{Namespace: "chosen"}, deps, "chart", "image")
			if (err == nil) != (failAt == "") {
				t.Fatalf("%s: %v", failAt, err)
			}
			if err == nil {
				if prepared.compatibility != testUpgradeCompatibility {
					t.Fatal(prepared.compatibility)
				}
			}
			if temporary != "" {
				if _, err := os.Stat(filepath.Dir(temporary)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("snapshot leaked: %v", err)
				}
			}
			if len(calls) < 3 || calls[0] != "history" || calls[1] != "get" || calls[2] != "get" {
				t.Fatal(calls)
			}
		})
	}
}

// Removed chart values are refused wherever they are set, never stripped:
// stored values, --values files and --set. --set <key>=null drops a stored
// one, and the offline render then sees it gone, as Helm's merge does.
func TestUpgradeRefusesRemovedChartValues(t *testing.T) {
	stored := map[string]any{"trustedTests": map[string]any{"runtime": "old"}, "vm": nil, "admission": map[string]any{"sizeWeights": map[string]any{"XL": 8}, "diskFloorBytes": 1}, "pushBannerURL": ""}
	file := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(file, []byte("maxConcurrentJobs: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := checkRemovedUpgradeValues(UpgradeConfig{UserValues: []string{file}, UserSet: []string{"argo.kubevirtCeiling.maxVMs=4"}}, stored)
	want := "chart schema validation failed: chart values that no longer exist are set: admission.sizeWeights (stored release values), argo.kubevirtCeiling (--set argo.kubevirtCeiling.maxVMs), maxConcurrentJobs (--values " + file + "), trustedTests (stored release values); no upgrade applied. " +
		"Delete them from any --values file and re-run the upgrade with --set admission.sizeWeights=null --set argo.kubevirtCeiling=null --set maxConcurrentJobs=null --set trustedTests=null (Helm drops a null value from the stored release values)"
	var schemaErr *ErrSchemaValidation
	if !errors.As(err, &schemaErr) || err.Error() != want {
		t.Fatalf("refusal = %v", err)
	}
	cfg := UpgradeConfig{UserSet: []string{"trustedTests=null", "admission.sizeWeights=null"}}
	if err := checkRemovedUpgradeValues(cfg, stored); err != nil {
		t.Fatalf("cleared values still refused: %v", err)
	}
	// Only the exact removed key set to null clears it.
	for _, set := range []string{"trustedTests.runtime=null", "trustedTests=", "trustedTests=false"} {
		if err := checkRemovedUpgradeValues(UpgradeConfig{UserSet: []string{set, "admission.sizeWeights=null"}}, stored); !errors.As(err, &schemaErr) {
			t.Fatalf("%s cleared a removed value: %v", set, err)
		}
	}
	dropClearedRemovedValues(cfg, stored)
	admission := stored["admission"].(map[string]any)
	if _, kept := stored["trustedTests"]; kept || admission["sizeWeights"] != nil || admission["diskFloorBytes"] != 1 || stored["pushBannerURL"] != "" {
		t.Fatalf("drop changed other values: %+v", stored)
	}
	chartValues := []byte("admission:\n  diskFloorBytes: 1\n")
	if err := validateUserSetAgainstValues(chartValues, cfg.UserSet); err != nil {
		t.Fatalf("clearing --set refused: %v", err)
	}
	if err := validateUserSetAgainstValues(chartValues, []string{"vm.kvm.enabled=true"}); !errors.As(err, &schemaErr) || !strings.Contains(err.Error(), "vm (--set vm.kvm.enabled)") || !strings.Contains(err.Error(), "--set vm=null") {
		t.Fatalf("removed --set not named: %v", err)
	}
}

// The chart test scripts strip the same removed values the upgrade refuses.
func TestStripScriptListsEveryRemovedChartValue(t *testing.T) {
	body, err := os.ReadFile("../../hack/strip-removed-chart-values.awk")
	if err != nil {
		t.Fatal(err)
	}
	want := `split("` + strings.Join(removedChartValues, " ") + `", list, " ")`
	if !strings.Contains(string(body), want) {
		t.Fatalf("hack/strip-removed-chart-values.awk lists other keys than removedChartValues: want %s", want)
	}
}

// Install re-runs and restores reuse the installed values; removed ones are
// refused there with the same names, pointing at oberth upgrade.
func TestReuseValuesPathsRefuseRemovedChartValues(t *testing.T) {
	installed := map[string]any{"auditAnchor": map[string]any{"rootsKey": "roots.pem"}, "upstream": map[string]any{"keySecret": "k", "name": "github"}, "vm": nil}
	err := refuseRemovedReleaseValues(installed, "oberth restore")
	want := "chart schema validation failed: the installed release values set chart values that no longer exist: auditAnchor.rootsKey, upstream.keySecret; oberth restore changed nothing. " +
		"Run oberth upgrade first: it names each stored one with the --set <key>=null flag that drops it, and replaces the installed chart's defaults"
	if err == nil || err.Error() != want {
		t.Fatalf("refusal = %v", err)
	}
	if err := refuseRemovedReleaseValues(map[string]any{"upstream": map[string]any{"name": "github"}}, "oberth install"); err != nil {
		t.Fatal(err)
	}
}
