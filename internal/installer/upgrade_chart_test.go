package installer

import (
	"encoding/json"
	"errors"
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
	out, err := exec.CommandContext(t.Context(), "../../hack/test-chart-upgrade.sh", "../../charts/oberth", containerimage.Utility).CombinedOutput()
	if err != nil {
		t.Fatalf("v0.17.53 values upgrade render: %v\n%s", err, out)
	}
}

func TestUpgradeMergesNewChartDefaults(t *testing.T) {
	args := upgradeHelmArgs(UpgradeConfig{Namespace: "oberth", BinaryVersion: "0.17.58"}, "oberth-charts/oberth", "example.invalid/server@sha256:fixture")
	if !slices.Contains(args, "--reset-then-reuse-values") || slices.Contains(args, "--reuse-values") || slices.Contains(args, "--reset-values") {
		t.Fatalf("upgrade must merge new defaults below old configuration and overrides: %v", args)
	}
}

// Exercise Helm's real upgrade merge against a saved prior release, without a
// live cluster or a dependency on Helm's Go SDK. Its memory driver substitutes
// a nonmutating kube client; HTTP supplies discovery metadata only.
func TestUpgradeActualHelmPreservesOldValuesAndAddsDefaults(t *testing.T) {
	old, err := os.ReadFile("../../hack/testdata/chart-values-v0.17.53.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := yaml.Unmarshal(old, &values); err != nil {
		t.Fatal(err)
	}
	values["pushBannerURL"] = ""
	values["retention"].(map[string]any)["keepNewestPerRef"] = 0
	values["argo"].(map[string]any)["goProxy"].(map[string]any)["enabled"] = false
	values["podAnnotations"] = map[string]any{"round17.example/stored": "kept"}
	dir := t.TempDir()
	release := []any{map[string]any{
		"name": "oberth", "namespace": "oberth", "version": 1,
		"info": map[string]any{"status": "deployed"},
		"chart": map[string]any{
			"metadata": map[string]any{"apiVersion": "v2", "name": "oberth", "version": "0.17.53"},
			"values":   values,
		},
		"config": values,
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
	args := upgradeHelmArgs(UpgradeConfig{Namespace: "oberth", ChartOverride: chart}, chart, image)
	args = append(args, "--dry-run=client", "--output=json", "--kubeconfig", kubeconfig)
	command := exec.CommandContext(t.Context(), "helm", args...)
	command.Env = append(os.Environ(), "HELM_DRIVER=memory", "HELM_MEMORY_DRIVER_DATA="+history,
		"HELM_CACHE_HOME="+dir, "HELM_CONFIG_HOME="+dir, "HELM_PLUGINS="+filepath.Join(dir, "plugins"))
	out, err := command.Output()
	if err != nil {
		var failure *exec.ExitError
		if errors.As(err, &failure) {
			t.Fatalf("actual Helm upgrade: %v\n%s", err, failure.Stderr)
		}
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
	for _, want := range []string{containerimage.Utility, "--git-backend=go-git", "round17.example/stored", "--retention-keep-newest=0", image} {
		if !strings.Contains(upgraded.Manifest, want) {
			t.Errorf("upgrade render lost default/configuration %q", want)
		}
	}
	// Template fallbacks can conceal a broken merge. The saved target chart
	// must also retain its new defaults for this and subsequent upgrades.
	if upgraded.Chart.Values["utilityImage"] != containerimage.Utility || upgraded.Chart.Values["gitBackend"] != "go-git" {
		t.Fatalf("upgrade discarded new chart defaults: utilityImage=%v gitBackend=%v", upgraded.Chart.Values["utilityImage"], upgraded.Chart.Values["gitBackend"])
	}
	proxy := upgraded.Config["argo"].(map[string]any)["goProxy"].(map[string]any)
	if proxy["enabled"] != false || upgraded.Config["pushBannerURL"] != "" {
		t.Fatalf("explicit false/empty configuration changed: %+v", upgraded.Config)
	}
}
