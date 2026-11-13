package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Built from v0.22.4 OberthHelmArgs, argoOberthHelmValues, upgradeHelmArgs,
// reconcileChangedDefaults and compatibilityOverlay, plus preserved operator
// proxy/federation settings. It is stored user configuration, not chart defaults.
func hotfixStoredValues(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/v0.22.4-installer-user-values.json")
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		t.Fatal(err)
	}
	return values
}

func hotfixHelm(t *testing.T, values map[string]any, output *bytes.Buffer) (Deps, string) {
	t.Helper()
	requireHelmForUpgradeTest(t)
	chart, err := filepath.Abs("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	deps := Deps{Output: output, RunHelm: func(ctx context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "version":
			return []byte("v4.2.3"), nil
		case "list":
			return []byte(`[{"name":"oberth","chart":"oberth-0.22.4","status":"deployed"}]`), nil
		case "history":
			return []byte(`[{"revision":1,"status":"deployed"}]`), nil
		case "get":
			if slices.Contains(args, "--all") {
				return json.Marshal(map[string]any{"compatibility": values["compatibility"], "defaultMustNotBecomeStored": true})
			}
			return bytes.Clone(raw), nil
		case "show", "template":
			cmd := exec.CommandContext(ctx, "helm", args...)
			data, err := cmd.CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("Helm validation: %s: %w", data, err)
			}
			return data, nil
		default:
			t.Fatalf("preview attempted a write: %v", args)
			return nil, nil
		}
	}, RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Fatal("preview attempted a command/write")
		return nil, nil
	}}
	return deps, chart
}

func TestUpgradeStoredInstallerValuesRenderAfterCleanup(t *testing.T) {
	values := hotfixStoredValues(t)
	proxy := values["argo"].(map[string]any)["goProxy"].(map[string]any)
	if proxy["enabled"] != true || proxy["modulePrefix"] == "" || proxy["upstream"] == "" || proxy["organization"] == "" {
		t.Fatal("historical fixture must exercise an enabled proxy and its stored module settings")
	}
	var output bytes.Buffer
	deps, chart := hotfixHelm(t, values, &output)
	prepared, err := prepareUpgradeValues(t.Context(), UpgradeConfig{Namespace: "oberth", ChartOverride: chart}, deps, chart, "example.invalid/server@sha256:"+strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	// Form the expected document independently of the cleanup implementation.
	delete(values["secretstore"].(map[string]any), "transit")
	argo := values["argo"].(map[string]any)
	delete(argo, "controllerProfile")
	delete(argo, "controllerProfileConfigMap")
	delete(argo, "executionPlacementConfigMap")
	delete(argo, "executionTargetConfigMap")
	delete(argo, "executionTargetCredentialsSecret")
	delete(argo, "goProxy")
	want, _ := json.Marshal(values)
	got, _ := json.Marshal(prepared.storedSnapshot)
	if !bytes.Equal(got, want) {
		t.Fatalf("another stored value changed: got %s want %s", got, want)
	}
	const omitted = "Omitted retired installer-written stored values: secretstore.transit.enabled, secretstore.transit.mount, secretstore.transit.key, argo.controllerProfile, argo.controllerProfileConfigMap, argo.executionPlacementConfigMap, argo.executionTargetConfigMap, argo.executionTargetCredentialsSecret\n"
	if strings.Count(output.String(), omitted) != 1 || strings.Count(output.String(), "Omitted retired installer-written") != 1 {
		t.Fatal("omission list differs", output.String())
	}
	if !strings.Contains(output.String(), "removed stored argo.goProxy configuration") {
		t.Fatal("proxy removal was not disclosed", output.String())
	}
}

func TestUpgradeDryRunValidatesCleanedStoredValues(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			values := hotfixStoredValues(t)
			if unknown {
				values["argo"].(map[string]any)["operatorUnknown"] = true
			}
			var output bytes.Buffer
			deps, chart := hotfixHelm(t, values, &output)
			_, err := RunUpgrade(t.Context(), UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.22.5", ChartOverride: chart, DryRun: true, NoBackup: true}, deps)
			if unknown {
				var schemaErr *ErrSchemaValidation
				if !errors.As(err, &schemaErr) || !strings.Contains(err.Error(), "operatorUnknown") {
					t.Fatalf("dry run missed schema failure: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestoreOldChartKeepsInstallerValues(t *testing.T) {
	values := hotfixStoredValues(t)
	before, _ := json.Marshal(values)
	deps := Deps{RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		if args[0] != "show" {
			t.Fatalf("old target must retain stored values: %v", args)
		}
		if args[1] == "chart" {
			return []byte("name: oberth\nversion: 0.22.4\n"), nil
		}
		return before, nil
	}}
	args, err := restoreHelmArgs(t.Context(), RestoreConfig{Namespace: "oberth", ChartPath: "old-chart"}, backupDeps{Deps: deps}, restoreTarget{Overlay: map[string]any{"compatibility": values["compatibility"]}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(values)
	if !bytes.Equal(before, after) || !slices.Contains(args, "--reuse-values") || slices.Contains(args, "--reset-values") {
		t.Fatal("old-chart restore rewrote owned values", args)
	}
}

func TestRetiredProfileCleanupKeepsOpaqueTransit(t *testing.T) {
	values := map[string]any{"argo": map[string]any{"controllerProfile": "retired-profile"}, "secretstore": map[string]any{"transit": "operator-configuration"}}
	if !dropRetiredInstallerValues(values, nil) || values["secretstore"].(map[string]any)["transit"] != "operator-configuration" {
		t.Fatal("profile cleanup silently pruned an unknown stored value")
	}
}

// Existing plan-format tests also need a verified chart and its public values
// now that dry run performs the same read-only validation as apply.
func withValidatedDryRunRelease(t testing.TB, deps *Deps, version string) {
	t.Helper()
	withSignedRelease(t, deps, version, fakeChartImageRef)
	run := deps.RunHelm
	deps.RunHelm = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) > 1 && args[0] == "show" && args[1] == "values" {
			return []byte("image:\n  ref: " + fakeChartImageRef + "\nreplicaCount: 1\npushBannerURL: ''\n"), nil
		}
		return run(ctx, args)
	}
}
