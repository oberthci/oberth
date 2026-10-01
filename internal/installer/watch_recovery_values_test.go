package installer

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func recoveryValuesFixture(t *testing.T) (Config, watchRecoveryPlan, []byte) {
	t.Helper()
	cfg, _, _, _, _ := recoveryUnitFixture(t)
	p := *cfg.watchRecovery
	p.Context, p.ClusterUID, p.ServerVersion = "reviewed", "cluster", "v1.36.3+k3s1"
	p.Release.RecordUID, p.Release.RecordRV = "failed-record", "99"
	p.Origin = &watchRecoveryOrigin{Confirmed: []watchAdopted{}}
	raw := []byte(`{"compatibility":{"schemaIdentity":"sample-schema-v1","auditDomain":"sample-audit-v1","witnessKeyInfo":"sample-witness-v1"},"argo":{"goProxy":{"modulePrefix":"go.example.org","repositoryPrefix":"sample-","upstream":"forge","organization":"sample"}}}`)
	path := filepath.Join(t.TempDir(), "public-values.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.ValuesFiles = []string{path}
	canonical, err := readWatchValuesMode(cfg.ValuesFiles, true)
	if err != nil {
		t.Fatal(err)
	}
	p.ValuesSHA256 = watchValuesDigest(canonical)
	cfg.WatchAdoptionPlan = filepath.Join(t.TempDir(), "plan.json")
	plan, _ := json.Marshal(p)
	if err := os.WriteFile(cfg.WatchAdoptionPlan, plan, 0600); err != nil {
		t.Fatal(err)
	}
	return cfg, p, canonical
}

func TestWatchRecoveryValuesPreserveOnlyPlanBoundNamespace(t *testing.T) {
	cfg, p, canonical := recoveryValuesFixture(t)
	if _, err := readWatchValues(cfg.ValuesFiles); err == nil {
		t.Fatal("v1 admitted non-connector values")
	}
	got, err := readWatchConfiguredValues(cfg)
	if err != nil || !bytes.Equal(got, canonical) {
		t.Fatalf("v2 rejected exact public compatibility: %v", err)
	}
	for _, group := range []string{"compatibility", "argo"} {
		t.Run("omit-"+group, func(t *testing.T) {
			var values map[string]any
			if json.Unmarshal(canonical, &values) != nil {
				t.Fatal("fixture decode failed")
			}
			delete(values, group)
			raw, _ := json.Marshal(values)
			if os.WriteFile(cfg.ValuesFiles[0], raw, 0600) != nil {
				t.Fatal("omission fixture write failed")
			}
			if _, err := readWatchConfiguredValues(cfg); err == nil {
				t.Fatal("planned public group omission accepted")
			}
		})
	}
	for _, value := range []string{"sample-schema-v1", "sample-audit-v1", "sample-witness-v1", "go.example.org", "sample-", "forge", "sample"} {
		t.Run(value, func(t *testing.T) {
			changed := bytes.Replace(canonical, []byte(`"`+value+`"`), []byte(`"`+strings.ToUpper(value)+`"`), 1)
			if bytes.Equal(changed, canonical) || os.WriteFile(cfg.ValuesFiles[0], changed, 0600) != nil {
				t.Fatal("mutation fixture failed")
			}
			if _, err := readWatchConfiguredValues(cfg); err == nil {
				t.Fatal("unplanned namespace field accepted")
			}
		})
	}
	if os.WriteFile(cfg.ValuesFiles[0], canonical, 0600) != nil {
		t.Fatal("restore fixture failed")
	}
	p.ValuesSHA256 = strings.Repeat("0", 64)
	plan, _ := json.Marshal(p)
	if os.WriteFile(cfg.WatchAdoptionPlan, plan, 0600) != nil {
		t.Fatal("changed plan fixture failed")
	}
	if _, err := readWatchConfiguredValues(cfg); err == nil {
		t.Fatal("mismatched public values hash accepted")
	}
}

func TestWatchRecoveryValuesRejectExtraPartialOrConflictingGroups(t *testing.T) {
	cfg, _, canonical := recoveryValuesFixture(t)
	for name, mutate := range map[string]func(map[string]any){
		"partial identities": func(v map[string]any) { delete(v["compatibility"].(map[string]any), "auditDomain") },
		"wrong type":         func(v map[string]any) { v["compatibility"].(map[string]any)["schemaIdentity"] = 12 },
		"null":               func(v map[string]any) { v["compatibility"] = nil },
		"invalid identity":   func(v map[string]any) { v["compatibility"].(map[string]any)["auditDomain"] = "bad\nidentity" },
		"proxy traversal": func(v map[string]any) {
			v["argo"].(map[string]any)["goProxy"].(map[string]any)["modulePrefix"] = "go.example.org/../x"
		},
		"proxy wildcard":  func(v map[string]any) { v["argo"].(map[string]any)["goProxy"].(map[string]any)["organization"] = "*" },
		"proxy enabled":   func(v map[string]any) { v["argo"].(map[string]any)["goProxy"].(map[string]any)["enabled"] = true },
		"proxy port":      func(v map[string]any) { v["argo"].(map[string]any)["goProxy"].(map[string]any)["port"] = 443 },
		"extra authority": func(v map[string]any) { v["argo"].(map[string]any)["vault"] = map[string]any{"role": "other"} },
		"image":           func(v map[string]any) { v["image"] = map[string]any{"ref": "other"} },
	} {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			if json.Unmarshal(canonical, &v) != nil {
				t.Fatal("fixture decode failed")
			}
			mutate(v)
			raw, _ := json.Marshal(v)
			if os.WriteFile(cfg.ValuesFiles[0], raw, 0600) != nil {
				t.Fatal("fixture write failed")
			}
			if _, err := readWatchValuesMode(cfg.ValuesFiles, true); err == nil {
				t.Fatal("unapproved public input accepted")
			}
		})
	}
	if os.WriteFile(cfg.ValuesFiles[0], []byte(`{"compatibility":{"schemaIdentity":"a","schemaIdentity":"b"}}`), 0600) != nil {
		t.Fatal("duplicate fixture write failed")
	}
	if _, err := readWatchValuesMode(cfg.ValuesFiles, true); err == nil {
		t.Fatal("duplicate values key accepted")
	}
	if os.WriteFile(cfg.ValuesFiles[0], canonical, 0600) != nil {
		t.Fatal("restore fixture failed")
	}
	second := filepath.Join(t.TempDir(), "second.json")
	changed := bytes.Replace(canonical, []byte("sample-audit-v1"), []byte("different-audit-v1"), 1)
	if os.WriteFile(second, changed, 0600) != nil {
		t.Fatal("second fixture write failed")
	}
	if _, err := readWatchValuesMode(append(cfg.ValuesFiles, second), true); err == nil {
		t.Fatal("conflicting multi-file namespace accepted")
	}
}

func TestWatchRecoveryValuesSealedInputSurvivesPathReplacement(t *testing.T) {
	cfg, p, canonical := recoveryValuesFixture(t)
	frozen, cleanup, err := freezeWatchValues(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if err = requireWatchRecoveryValues(frozen, p); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(t.TempDir(), "replacement.json")
	if os.WriteFile(replacement, []byte(`{"watchTunnel":{"enabled":false}}`), 0600) != nil || os.Rename(replacement, cfg.ValuesFiles[0]) != nil {
		t.Fatal("path replacement failed")
	}
	if err = requireWatchRecoveryValues(frozen, p); err != nil {
		t.Fatal("sealed identity changed after replacement", err)
	}
	got, err := os.ReadFile(frozen.ValuesFiles[0])
	if err != nil || !bytes.Equal(got, canonical) {
		t.Fatal("Helm source bytes changed")
	}
	if os.WriteFile(frozen.ValuesFiles[0], []byte(`{}`), 0600) == nil {
		t.Fatal("sealed values writable")
	}
	frozen.ValuesFiles = cfg.ValuesFiles
	if requireWatchRecoveryValues(frozen, p) == nil {
		t.Fatal("mutable path substitution accepted")
	}
}

func TestWatchRecoveryAPIVersionBoundedToQualifiedPatches(t *testing.T) {
	for _, version := range []string{"v1.36.2", "v1.36.3+k3s1"} {
		if !supportedWatchRecoveryAPI(version) {
			t.Fatal("qualified version refused")
		}
	}
	for _, version := range []string{"v1.35.9", "v1.36.1", "v1.36.4", "v1.37.0", "v1.36.3-rc.1"} {
		if supportedWatchRecoveryAPI(version) {
			t.Fatal("unqualified API version admitted")
		}
	}
	cfg, _, _ := recoveryValuesFixture(t)
	p, err := readWatchRecoveryPlan(cfg)
	if err != nil || p.ServerVersion != "v1.36.3+k3s1" || !p.ExpiresAt.After(time.Now()) {
		t.Fatal("exact API version was not retained", err)
	}
}
