package installer

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRetiredInstallerCleanupMatchesHistoricalSnapshot(t *testing.T) {
	var old, cleaned map[string]any
	for _, f := range []struct {
		name string
		dst  *map[string]any
	}{{"v0.22.3-values.yaml.golden", &old}, {"v0.22.3-cleaned-values.yaml.golden", &cleaned}} {
		body, err := os.ReadFile("testdata/" + f.name)
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(body, f.dst); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if !dropRetiredInstallerValues(old, nil, &output) || !reflect.DeepEqual(old, cleaned) {
		t.Fatal("historical snapshot cleanup differs from the chart fixture")
	}
	if strings.Count(output.String(), "\n") != 1 {
		t.Fatal("cleanup must emit one note", output.String())
	}
	before := output.String()
	if dropRetiredInstallerValues(old, nil, &output) || output.String() != before {
		t.Fatal("cleanup is not idempotent")
	}
}

func TestRetiredInstallerCleanupKeepsEveryOtherKey(t *testing.T) {
	live := map[string]any{"enabled": true, "role": "operator-role", "transit": map[string]any{"enabled": false, "mount": "old-mount", "key": "old-key", "operatorUnknown": "must-be-validated"}}
	values := map[string]any{"secretstore": live, "argo": map[string]any{"goProxy": map[string]any{"enabled": false}}, "unknownOperatorFeature": true}
	var output bytes.Buffer
	if !dropRetiredInstallerValues(values, nil, &output) {
		t.Fatal("retired keys not found")
	}
	if live["enabled"] != true || live["role"] != "operator-role" || values["unknownOperatorFeature"] != true || !reflect.DeepEqual(live["transit"], map[string]any{"operatorUnknown": "must-be-validated"}) || values["argo"].(map[string]any)["goProxy"].(map[string]any)["enabled"] != false {
		t.Fatal("cleanup changed an unowned or live value")
	}
	if strings.Contains(output.String(), "old-key") || strings.Contains(output.String(), "old-mount") {
		t.Fatal("cleanup printed stored values")
	}
}

func TestRestoreRetiredValuesFollowTargetChart(t *testing.T) {
	requireHelmForUpgradeTest(t)
	for _, tc := range []struct {
		name, chart        string
		transit, execution bool
	}{
		{"v0.22.4", "testdata/oberth-v0.22.4-chart.tgz", true, true},
		{"v0.22.6", "testdata/oberth-v0.22.6-chart.tgz", false, true},
		{"current", "../../charts/oberth", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := hotfixStoredValues(t)
			delete(values["argo"].(map[string]any), "goProxy") // Separate explicit restore refusal.
			var output bytes.Buffer
			deps, _ := hotfixHelm(t, values, &output)
			dir := t.TempDir()
			identities := map[string]any{"schemaIdentity": "archive-schema", "auditDomain": "archive-audit", "witnessKeyInfo": "archive-witness"}
			args, err := restoreHelmArgs(t.Context(), RestoreConfig{Namespace: "oberth", ChartPath: tc.chart}, backupDeps{Deps: deps}, restoreTarget{Overlay: map[string]any{"compatibility": identities}}, dir)
			if err != nil {
				t.Fatal(err)
			}
			render := []string{"template", "oberth", tc.chart, "-n", "oberth"}
			snapshot := values
			if slices.Contains(args, "--reuse-values") {
				raw, _ := json.Marshal(values)
				file := filepath.Join(dir, "reused.json")
				if err := os.WriteFile(file, raw, 0600); err != nil {
					t.Fatal(err)
				}
				render = append(render, "-f", file)
			} else {
				if !slices.Contains(args, "--reset-values") {
					t.Fatal("cleanup must reset old defaults", args)
				}
				raw, err := os.ReadFile(args[slices.Index(args, "--values")+1])
				if err != nil {
					t.Fatal(err)
				}
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.UseNumber()
				if err := decoder.Decode(&snapshot); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-f" || args[i] == "--values" {
					render = append(render, args[i], args[i+1])
					i++
				}
			}
			body, err := exec.CommandContext(t.Context(), "helm", render...).CombinedOutput()
			if err != nil {
				t.Fatalf("restore target render: %v: %s", err, body)
			}
			// Build the entire expected snapshot independently of the cleanup code.
			want := hotfixStoredValues(t)
			argo := want["argo"].(map[string]any)
			delete(argo, "goProxy")
			if !tc.transit {
				delete(want["secretstore"].(map[string]any), "transit")
				delete(argo, "controllerProfile")
				delete(argo, "controllerProfileConfigMap")
			}
			if !tc.execution {
				delete(argo, "executionPlacementConfigMap")
				delete(argo, "executionTargetConfigMap")
				delete(argo, "executionTargetCredentialsSecret")
			}
			if !reflect.DeepEqual(snapshot, want) {
				t.Fatalf("target-owned or unrelated values changed: got %#v want %#v", snapshot, want)
			}
			if !strings.Contains(string(body), "archive-schema") {
				t.Fatal("restore overlay lost precedence")
			}
			// Unrelated unknown values still reach target schema validation.
			file := filepath.Join(dir, "unknown.json")
			if err := os.WriteFile(file, []byte(`{"argo":{"operatorUnknown":true}}`), 0600); err != nil {
				t.Fatal(err)
			}
			refused, err := exec.CommandContext(t.Context(), "helm", append(render, "-f", file)...).CombinedOutput()
			if err == nil || !strings.Contains(string(refused), "operatorUnknown") {
				t.Fatal("unknown operator key was silently removed", err, string(refused))
			}
		})
	}
}

func TestRetiredInstallerCleanupKeepsIndividualTargetLeaves(t *testing.T) {
	values := map[string]any{"secretstore": map[string]any{"transit": map[string]any{"enabled": true, "mount": "old", "key": "old", "operatorUnknown": true}}, "argo": map[string]any{"controllerProfile": "old", "controllerProfileConfigMap": "old", "executionTargetConfigMap": "target"}}
	target := map[string]any{"secretstore": map[string]any{"transit": map[string]any{"enabled": false}}, "argo": map[string]any{"controllerProfile": nil}}
	if !dropRetiredInstallerValues(values, target, nil) {
		t.Fatal("no cleanup")
	}
	want := map[string]any{"secretstore": map[string]any{"transit": map[string]any{"enabled": true, "operatorUnknown": true}}, "argo": map[string]any{"controllerProfile": "old"}}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("leaf cleanup differs: %#v", values)
	}
}
