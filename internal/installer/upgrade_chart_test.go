package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/containerimage"
)

// Use the same old-release render gate as chart/release CI, checking its
// utility fallback against the server maintenance helper's shared pin too.
func TestUpgradeChartFromV01753Values(t *testing.T) {
	requireHelmForUpgradeTest(t)
	out, err := exec.CommandContext(t.Context(), "../../hack/test-chart-upgrade.sh", "../../charts/oberth", containerimage.Utility).CombinedOutput()
	if err != nil {
		t.Fatalf("v0.17.53 values upgrade render: %v\n%s", err, out)
	}
}

func TestUpgradeMergesNewChartDefaults(t *testing.T) {
	args, flags := upgradeHelmArgs(UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.17.58"}, "oberth-charts/oberth", "example.invalid/server@sha256:fixture", testUpgradeCompatibility)
	args = append(args, flags...)
	if !slices.Contains(args, "--reset-then-reuse-values") || slices.Contains(args, "--reuse-values") || slices.Contains(args, "--reset-values") {
		t.Fatalf("upgrade must merge new defaults below old configuration and overrides: %v", args)
	}
}

// Exercise Helm's real upgrade merge against a saved prior release, without a
// live cluster or a dependency on Helm's Go SDK. Its memory driver substitutes
// a nonmutating kube client; HTTP supplies discovery metadata only.
func TestUpgradeActualHelmPreservesOldValuesAndAddsDefaults(t *testing.T) {
	for _, version := range []string{"0.17.53", "0.17.62"} {
		t.Run(version, func(t *testing.T) { testUpgradeActualHelmPreservesOldValues(t, version) })
	}
}

func testUpgradeActualHelmPreservesOldValues(t *testing.T, oldVersion string) {
	requireHelmForUpgradeTest(t)
	old, err := os.ReadFile("../../hack/testdata/chart-values-v" + oldVersion + ".yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(old, &values); err != nil {
		t.Fatal(err)
	}
	if oldVersion == "0.17.62" {
		cache, ok := values["gitcache"].(map[string]any)
		if values["gitBackend"] != "cli" || !ok || cache["shadow"] != false {
			t.Fatal("v0.17.62 fixture lost raw legacy defaults")
		}
	}
	values["removedOldDefault"] = "not user configuration"
	values["compatibility"] = map[string]any{"schemaIdentity": testUpgradeCompatibility.SchemaIdentity, "auditDomain": testUpgradeCompatibility.AuditDomain, "witnessKeyInfo": testUpgradeCompatibility.WitnessKeyInfo}
	config := map[string]any{"image": map[string]any{"ref": "example.invalid/old@sha256:" + strings.Repeat("2", 64)}, "argo": map[string]any{"enabled": true, "goProxy": map[string]any{"enabled": false}}, "pushBannerURL": "", "retention": map[string]any{"keepNewestPerRef": 0}, "podAnnotations": map[string]any{"round17.example/stored": "kept"}}
	values["pushBannerURL"] = ""
	values["retention"].(map[string]any)["keepNewestPerRef"] = 0
	values["argo"].(map[string]any)["goProxy"].(map[string]any)["enabled"] = false
	values["podAnnotations"] = map[string]any{"round17.example/stored": "kept"}
	dir := t.TempDir()
	release := []any{map[string]any{
		"name": "oberth", "namespace": "oberth", "version": 1,
		"info": map[string]any{"status": "deployed"},
		"chart": map[string]any{
			"metadata": map[string]any{"apiVersion": "v2", "name": "oberth", "version": oldVersion},
			"values":   values,
		},
		"config": config,
	}}
	encoded, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(dir, "release.json")
	if err := os.WriteFile(history, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			t.Errorf("dry-run attempted a Kubernetes mutation: %s %s", r.Method, r.URL.Path)
			http.Error(w, "mutations refused", http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/version":
			_, _ = io.WriteString(w, `{"major":"1","minor":"36","gitVersion":"v1.36.0"}`)
		case "/api":
			_, _ = io.WriteString(w, `{"kind":"APIVersions","apiVersion":"v1","versions":["v1"]}`)
		case "/apis":
			_, _ = io.WriteString(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`)
		case "/api/v1":
			_, _ = io.WriteString(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[]}`)
		default:
			t.Errorf("unexpected discovery request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	body := "apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: " + server.URL + "\ncontexts:\n- name: fixture\n  context:\n    cluster: fixture\n    namespace: oberth\ncurrent-context: fixture\n"
	if err := os.WriteFile(kubeconfig, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	chart, err := filepath.Abs("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	image := "example.invalid/oberth@sha256:" + strings.Repeat("1", 64)
	runHelm := func(ctx context.Context, args []string) ([]byte, error) {
		args = append(append([]string{}, args...), "--kubeconfig", kubeconfig)
		command := exec.CommandContext(ctx, "helm", args...)
		command.Env = append(os.Environ(), "HELM_DRIVER=memory", "HELM_MEMORY_DRIVER_DATA="+history, "HELM_CACHE_HOME="+dir, "HELM_CONFIG_HOME="+dir, "HELM_PLUGINS="+filepath.Join(dir, "plugins"))
		out, err := command.Output()
		if err != nil {
			var failure *exec.ExitError
			if errors.As(err, &failure) {
				return nil, fmt.Errorf("Helm: %w: %s", err, failure.Stderr)
			}
		}
		return out, err
	}
	cfg := UpgradeConfig{Namespace: "oberth", ChartOverride: chart}
	deps := Deps{RunHelm: runHelm}
	if err := requireUpgradeHelmVersion(t.Context(), deps); err != nil {
		t.Fatal(err)
	}
	compat, err := prepareUpgradeValues(t.Context(), cfg, deps, chart, image)
	if err != nil {
		t.Fatal(err)
	}
	if compat != testUpgradeCompatibility {
		t.Fatalf("identities from installed chart defaults lost: %+v", compat)
	}
	args, flags := upgradeHelmArgs(cfg, chart, image, compat)
	args = append(args, flags...)
	args = append(args, "--dry-run=client", "--output=json")
	out, err := runHelm(t.Context(), args)
	if err != nil {
		t.Fatal(err)
	}

	var upgraded struct {
		Manifest string         `json:"manifest"`
		Config   map[string]any `json:"config"`
		Chart    struct {
			Values map[string]any `json:"values"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(out, &upgraded); err != nil {
		t.Fatalf("decode actual upgrade: %v\n%s", err, out)
	}
	for _, forbidden := range []string{"--git-backend", "OBERTH_GITCACHE_SHADOW"} {
		if strings.Contains(upgraded.Manifest, forbidden) {
			t.Fatalf("legacy field reactivated %s", forbidden)
		}
	}
	for _, want := range []string{containerimage.Utility, "round17.example/stored", "--retention-keep-newest=0", image} {
		if !strings.Contains(upgraded.Manifest, want) {
			t.Errorf("upgrade render lost default/configuration %q", want)
		}
	}
	// Template fallbacks can conceal a broken merge. The saved target chart
	// must also retain its new defaults for this and subsequent upgrades.
	if upgraded.Chart.Values["utilityImage"] != containerimage.Utility {
		t.Fatalf("upgrade discarded new chart defaults: utilityImage=%v", upgraded.Chart.Values["utilityImage"])
	}
	for _, want := range []string{compat.SchemaIdentity, compat.AuditDomain, compat.WitnessKeyInfo} {
		if !strings.Contains(upgraded.Manifest, want) {
			t.Errorf("render dropped installed identity %q", want)
		}
	}
	proxy := upgraded.Config["argo"].(map[string]any)["goProxy"].(map[string]any)
	if proxy["enabled"] != false || upgraded.Config["pushBannerURL"] != "" {
		t.Fatalf("explicit false/empty configuration changed: %+v", upgraded.Config)
	}
	// Install re-runs and restore deliberately retain old chart defaults. Both
	// paths must render every value introduced since v0.17.53 via fallbacks.
	// Remove the synthetic old-only default for raw reuse: strict target schemas
	// correctly reject it, which is why this mode needs an exact chart contract.
	delete(values, "removedOldDefault")
	encoded, err = json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(history, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"install", "restore"} {
		t.Run(mode, func(t *testing.T) {
			var args []string
			if mode == "install" {
				args = OberthHelmArgs(Config{Namespace: "oberth", ChartPath: chart, ImageRef: image}, OpenBaoResult{}, RekorResult{})
			} else {
				args, err = restoreHelmArgs(t.Context(), RestoreConfig{Namespace: "oberth", ChartPath: chart}, backupDeps{Deps: deps}, restoreTarget{Overlay: map[string]any{"compatibility": values["compatibility"]}}, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
			}
			if !slices.Contains(args, "--reuse-values") {
				t.Fatal("install/restore no longer preserve old chart defaults")
			}
			out, err := runHelm(t.Context(), append(args, "--dry-run=client", "--output=json"))
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Manifest string `json:"manifest"`
			}
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"--git-backend", "OBERTH_GITCACHE_SHADOW"} {
				if strings.Contains(result.Manifest, forbidden) {
					t.Fatalf("legacy field reactivated %s", forbidden)
				}
			}
			for _, want := range []string{containerimage.Utility, compat.SchemaIdentity, compat.AuditDomain, compat.WitnessKeyInfo} {
				if !strings.Contains(result.Manifest, want) {
					t.Errorf("%s raw-reuse render lacks %q", mode, want)
				}
			}
		})
	}
	// A user-set removed key must be refused by the TARGET schema before upgrade.
	// Exercise top-level and nested additionalProperties=false with real Helm.
	for _, nested := range []bool{false, true} {
		parent := config
		if nested {
			parent = config["retention"].(map[string]any)
		}
		parent["removedByTarget"] = true
		encoded, err = json.Marshal(release)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(history, encoded, 0600); err != nil {
			t.Fatal(err)
		}
		_, err = prepareUpgradeValues(t.Context(), cfg, deps, chart, image)
		var schemaErr *ErrSchemaValidation
		if !errors.As(err, &schemaErr) || !strings.Contains(err.Error(), "removedByTarget") {
			t.Fatalf("unknown stored key reached upgrade: %v", err)
		}
		delete(parent, "removedByTarget")
	}

}

var testUpgradeCompatibility = backupCompatibility{SchemaIdentity: "legacy-schema-v1", AuditDomain: "legacy-audit-v1", WitnessKeyInfo: "legacy-witness-v1"}

func requireHelmForUpgradeTest(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm is not on PATH; chart and real-Helm upgrade tests run in pinned CI")
	}
}

// Incidental preflight reads for tests focused on other upgrade boundaries.
// Dedicated value/version tests below exercise failures and ordering directly.
func testUpgradeValuePreflight(args []string) ([]byte, bool) {
	if len(args) == 0 {
		return nil, false
	}
	if args[0] == "version" {
		return []byte("v4.2.3"), true
	}
	if args[0] == "template" {
		return nil, true
	}
	if len(args) >= 3 && args[0] == "get" && args[1] == "values" && args[2] == "oberth" {
		if slices.Contains(args, "--all") {
			return []byte(`{"compatibility":{"schemaIdentity":"legacy-schema-v1","auditDomain":"legacy-audit-v1","witnessKeyInfo":"legacy-witness-v1"}}`), true
		}
		return []byte(`{}`), true
	}
	return nil, false
}
