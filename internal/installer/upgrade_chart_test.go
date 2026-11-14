package installer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/oberthci/oberth/internal/containerimage"
)

// Use the same old-release render gate as chart/release CI, checking its
// utility fallback against the server maintenance helper's shared pin too.
func TestUpgradeChartUsesCurrentValues(t *testing.T) {
	requireHelmForUpgradeTest(t)
	out, err := exec.CommandContext(t.Context(), "../../hack/test-chart-upgrade.sh", "../../charts/oberth", containerimage.Utility).CombinedOutput()
	if err != nil {
		t.Fatalf("chart upgrade render: %v\n%s", err, out)
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
func TestUpgradeActualHelmPreservesValuesAndAddsDefaults(t *testing.T) {
	requireHelmForUpgradeTest(t)
	old, err := os.ReadFile("testdata/v0.22.3-values.yaml.golden")
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(old, &values); err != nil {
		t.Fatal(err)
	}
	values["removedOldDefault"] = "not user configuration"
	values["compatibility"] = map[string]any{"schemaIdentity": testUpgradeCompatibility.SchemaIdentity, "auditDomain": testUpgradeCompatibility.AuditDomain, "witnessKeyInfo": testUpgradeCompatibility.WitnessKeyInfo}
	var config map[string]any
	stored, err := os.ReadFile("testdata/v0.22.3-stored-config.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stored, &config); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	release := []any{map[string]any{
		"name": "oberth", "namespace": "oberth", "version": 1,
		"info": map[string]any{"status": "deployed"},
		"chart": map[string]any{
			"metadata": map[string]any{"apiVersion": "v2", "name": "oberth", "version": "0.22.3"},
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
	chart, err := filepath.Abs("../../charts/oberth")
	if err != nil {
		t.Fatal(err)
	}
	image := "example.invalid/oberth@sha256:" + strings.Repeat("1", 64)
	runHelm := upgradeTestHelm(t, dir, history)
	cfg := UpgradeConfig{Namespace: "oberth", ChartOverride: chart}
	deps := Deps{RunHelm: runHelm}
	if err := requireUpgradeHelmVersion(t.Context(), deps); err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareUpgradeValues(t.Context(), cfg, deps, chart, image)
	if err != nil {
		t.Fatal(err)
	}
	compat := prepared.compatibility
	if compat != testUpgradeCompatibility {
		t.Fatalf("identities from installed chart defaults lost: %+v", compat)
	}
	applyCfg := cfg
	cleanup, err := prepared.configureStoredSnapshot(&applyCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	args, flags := upgradeHelmArgs(applyCfg, chart, image, compat)
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
	if _, exists := upgraded.Config["secretstore"].(map[string]any)["transit"]; exists {
		t.Fatal("retired installer values survived the actual upgrade")
	}
	if access, exists := upgraded.Config["sourceAccess"]; !exists || access != nil {
		t.Fatal("explicit null sourceAccess changed")
	}
	if _, exists := upgraded.Config["argo"].(map[string]any)["goProxy"]; exists {
		t.Fatal("retired proxy configuration survived actual upgrade")
	}
	if upgraded.Config["pushBannerURL"] != "" {
		t.Fatal("explicit empty configuration changed")
	}
	// Proxy retirement is proved above. The remaining historical fixtures
	// exercise retained settings independently of this removed subtree.
	dropStoredGoProxy(values, nil)
	dropStoredGoProxy(config, nil)
	retained, err := json.Marshal(release)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(history, retained, 0600); err != nil {
		t.Fatal(err)
	}
	fixture := historicalHelmUpgrade{cfg: cfg, deps: deps, values: values, config: config, release: release, history: history, chart: chart, image: image}
	fixture.checkOperatorOverrides(t, applyCfg, compat)
	fixture.checkInstallRestore(t)
	fixture.checkHistoricalRestore(t)
	fixture.checkDefaultTransit(t)
	fixture.checkUnknownStoredValues(t)

}

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
	if out, ok := testUpgradeArgoSecurityPreflight(args); ok {
		return out, true
	}
	if args[0] == "upgrade" && slices.Contains(args, "--dry-run=server") {
		return []byte(`{"config":{}}`), true
	}
	if args[0] == "history" {
		return []byte(`[{"revision":1,"status":"deployed"}]`), true
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

func testUpgradeArgoSecurityPreflight(args []string) ([]byte, bool) {
	if len(args) == 0 {
		return nil, false
	}
	if args[0] == "list" && slices.Contains(args, DefaultArgoNamespace) {
		return []byte(`[{"name":"argo-workflows","namespace":"oberth-argo","status":"deployed","chart":"argo-workflows-1.0.24"}]`), true
	}
	if len(args) >= 3 && args[0] == "get" && args[1] == "values" && args[2] == "argo-workflows" {
		flags, _, err := argoSecurityDefaults(nil)
		if err != nil {
			panic(err)
		}
		values := map[string]any{}
		for i := 0; i+1 < len(flags); i += 2 {
			key, raw, _ := strings.Cut(flags[i+1], "=")
			var value any
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				panic(err)
			}
			setAbsentArgoSecurityValue(values, strings.Split(key, "."), value)
		}
		body, err := json.Marshal(values)
		if err != nil {
			panic(err)
		}
		return body, true
	}

	return nil, false
}

// Only local discovery reads are served; Helm uses its nonmutating memory driver.
func upgradeTestHelm(t *testing.T, dir, history string) func(context.Context, []string) ([]byte, error) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(server.Close)
	kubeconfig := filepath.Join(dir, "kubeconfig")
	caData := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	body := "apiVersion: v1\nkind: Config\nclusters:\n- name: fixture\n  cluster:\n    server: " + server.URL + "\n    certificate-authority-data: " + caData + "\ncontexts:\n- name: fixture\n  context:\n    cluster: fixture\n    namespace: oberth\ncurrent-context: fixture\n"
	if err := os.WriteFile(kubeconfig, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(ctx context.Context, args []string) ([]byte, error) {
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
}

var testUpgradeCompatibility = backupCompatibility{SchemaIdentity: "legacy-schema-v1", AuditDomain: "legacy-audit-v1", WitnessKeyInfo: "legacy-witness-v1"}

type historicalHelmUpgrade struct {
	cfg                   UpgradeConfig
	deps                  Deps
	values, config        map[string]any
	release               []any
	history, chart, image string
}

func (f historicalHelmUpgrade) checkOperatorOverrides(t *testing.T, applyCfg UpgradeConfig, compat backupCompatibility) {
	t.Helper()
	chart, image, runHelm := f.chart, f.image, f.deps.RunHelm
	t.Run("operator-override-precedence", func(t *testing.T) {
		override := filepath.Join(t.TempDir(), "operator.yaml")
		if err := os.WriteFile(override, []byte("retention:\n  keepNewestPerRef: 7\n"), 0600); err != nil {
			t.Fatal(err)
		}
		overridden := applyCfg
		overridden.UserValues = []string{override}
		overridden.UserSet = []string{"retention.keepNewestPerRef=9"}
		a, f := upgradeHelmArgs(overridden, chart, image, compat)
		raw, err := runHelm(t.Context(), append(append(a, f...), "--dry-run=client", "--output=json"))
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if result.Config["retention"].(map[string]any)["keepNewestPerRef"] != float64(9) {
			t.Fatal("stored snapshot overrode operator values")
		}
	})
	t.Run("new-retired-override-refused", func(t *testing.T) {
		invalid := applyCfg
		invalid.UserSet = []string{"secretstore.transit.enabled=false"}
		a, f := upgradeHelmArgs(invalid, chart, image, compat)
		if _, err := runHelm(t.Context(), append(append(a, f...), "--dry-run=client", "--output=json")); err == nil {
			t.Fatal("new retired override was silently dropped")
		}
	})

}

func (f historicalHelmUpgrade) checkInstallRestore(t *testing.T) {
	t.Helper()
	values := f.values
	chart, image, deps := f.chart, f.image, f.deps
	runHelm := deps.RunHelm
	// Old defaults, including the synthetic removed key, must never become
	// stored user configuration during either path.
	for _, mode := range []string{"install", "restore"} {
		t.Run(mode, func(t *testing.T) {
			var args []string
			var err error
			identities := values["compatibility"]
			if mode == "install" {
				args = OberthHelmArgs(Config{Namespace: "oberth", ChartPath: chart, ImageRef: image}, OpenBaoResult{}, RekorResult{})
			} else {
				// The restored database can have entirely different identities.
				// Helm --set-string pins would beat this later values overlay.
				identities = map[string]any{"schemaIdentity": "archive-schema-v1", "auditDomain": "archive-audit-v1", "witnessKeyInfo": "archive-witness-v1"}
				args, err = restoreHelmArgs(t.Context(), RestoreConfig{Namespace: "oberth", ChartPath: chart}, backupDeps{Deps: deps}, restoreTarget{Overlay: map[string]any{"compatibility": identities}}, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "install" {
				args, err = prepareRetiredInstallerReuse(t.Context(), deps, "oberth", t.TempDir(), args, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			if slices.Contains(args, "--reuse-values") || !slices.Contains(args, "--reset-values") {
				t.Fatal("retired defaults would be reintroduced by raw reuse", args)
			}
			out, err := runHelm(t.Context(), append(args, "--dry-run=client", "--output=json"))
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Manifest string         `json:"manifest"`
				Config   map[string]any `json:"config"`
			}
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatal(err)
			}
			// Assert the entire stored map, including nested keys. Chart defaults
			// may influence the render but must not enter release.Config.
			stored, err := json.Marshal(f.config)
			if err != nil {
				t.Fatal(err)
			}
			var want map[string]any
			if err := json.Unmarshal(stored, &want); err != nil {
				t.Fatal(err)
			}
			dropRetiredInstallerValues(want, nil, nil)
			want["compatibility"] = identities
			if mode == "install" {
				want["image"].(map[string]any)["ref"] = image
				argo := want["argo"].(map[string]any)
				argo["namespace"] = DefaultArgoNamespace
			}
			if !reflect.DeepEqual(result.Config, want) {
				t.Fatalf("stored user key set changed: got %#v, want %#v", result.Config, want)
			}
			if access, exists := result.Config["sourceAccess"]; !exists || access != nil {
				t.Fatal("explicit null sourceAccess changed", mode)
			}
			if _, exists := result.Config["secretstore"].(map[string]any)["transit"]; exists {
				t.Fatal("retired installer values survived", mode)
			}
			if result.Config["pushBannerURL"] != "" || result.Config["retention"].(map[string]any)["keepNewestPerRef"] != float64(0) {
				t.Fatal("live operator values changed", mode)
			}
			for _, forbidden := range []string{"--git-backend", "OBERTH_GITCACHE_SHADOW"} {
				if strings.Contains(result.Manifest, forbidden) {
					t.Fatalf("legacy field reactivated %s", forbidden)
				}
			}
			expectedIdentities := identities.(map[string]any)
			for _, want := range []string{containerimage.Utility, expectedIdentities["schemaIdentity"].(string), expectedIdentities["auditDomain"].(string), expectedIdentities["witnessKeyInfo"].(string)} {
				if !strings.Contains(result.Manifest, want) {
					t.Errorf("%s raw-reuse render lacks %q", mode, want)
				}
			}
		})
	}

}

func (f historicalHelmUpgrade) checkDefaultTransit(t *testing.T) {
	t.Helper()
	t.Run("transit-only-in-chart-defaults", func(t *testing.T) {
		store := f.config["secretstore"].(map[string]any)
		transit := store["transit"]
		defer func() { store["transit"] = transit }()
		delete(store, "transit")
		body, err := json.Marshal(f.release)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.history, body, 0600); err != nil {
			t.Fatal(err)
		}
		f.checkInstallRestore(t)
	})
}

// A restore of the installed v0.22.3 chart must keep its active Transit
// values. Cleaning them would silently select that chart's disabled default.
func (f historicalHelmUpgrade) checkHistoricalRestore(t *testing.T) {
	t.Helper()
	t.Run("restore-v0.22.3", func(t *testing.T) {
		chart := t.TempDir()
		values, err := yaml.Marshal(f.values)
		if err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string][]byte{
			"Chart.yaml":  []byte("apiVersion: v2\nname: oberth\nversion: 0.22.3\n"),
			"values.yaml": values,
		} {
			if err := os.WriteFile(filepath.Join(chart, name), body, 0600); err != nil {
				t.Fatal(err)
			}
		}
		args, err := restoreHelmArgs(t.Context(), RestoreConfig{Namespace: "oberth", ChartPath: chart}, backupDeps{Deps: f.deps}, restoreTarget{Overlay: map[string]any{"compatibility": f.values["compatibility"]}}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(args, "--reuse-values") || slices.Contains(args, "--reset-values") {
			t.Fatal("restore cleaned values still owned by the installed chart", args)
		}
		out, err := f.deps.RunHelm(t.Context(), append(args, "--dry-run=client", "--output=json"))
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatal(err)
		}
		transit := result.Config["secretstore"].(map[string]any)["transit"]
		if !reflect.DeepEqual(transit, f.config["secretstore"].(map[string]any)["transit"]) {
			t.Fatal("restore changed the installed chart's Transit configuration", transit)
		}
	})
}

func (f historicalHelmUpgrade) checkUnknownStoredValues(t *testing.T) {
	t.Helper()
	config, release, history := f.config, f.release, f.history
	cfg, deps, chart, image := f.cfg, f.deps, f.chart, f.image
	// A user-set removed key must be refused by the TARGET schema before upgrade.
	// Exercise top-level and nested additionalProperties=false with real Helm.
	for _, nested := range []string{"top", "retention", "transit"} {
		parent := config
		if nested == "retention" {
			parent = config["retention"].(map[string]any)
		}
		if nested == "transit" {
			parent = config["secretstore"].(map[string]any)["transit"].(map[string]any)
		}
		parent["removedByTarget"] = true
		encoded, err := json.Marshal(release)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(history, encoded, 0600); err != nil {
			t.Fatal(err)
		}
		_, err = prepareUpgradeValues(t.Context(), cfg, deps, chart, image)
		var schemaErr *ErrSchemaValidation
		wantKey := "removedByTarget"
		if nested == "transit" {
			wantKey = "transit"
		}
		if !errors.As(err, &schemaErr) || !strings.Contains(err.Error(), wantKey) {
			t.Fatalf("unknown stored key reached upgrade: %v", err)
		}
		delete(parent, "removedByTarget")
	}
}
