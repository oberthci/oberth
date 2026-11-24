package installer_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/installer/contracttest"
	"github.com/oberthci/oberth/internal/installer/fake"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

func chartPath() string { return filepath.Join("..", "..", "charts", "oberth") }
func rendered(t *testing.T, values map[string]any) []map[string]any {
	t.Helper()
	return renderedIn(t, values, "oberth")
}
func renderedIn(t *testing.T, values map[string]any, namespace string) []map[string]any {
	t.Helper()
	ch, err := loader.Load(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	files, err := installer.Render(ch, values, namespace)
	if err != nil {
		t.Fatal(err)
	}
	var objects []map[string]any
	for name, raw := range files {
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		dec := kyaml.NewYAMLOrJSONDecoder(strings.NewReader(raw), 4096)
		for {
			var object map[string]any
			err = dec.Decode(&object)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(name, err)
			}
			if len(object) > 0 {
				objects = append(objects, object)
			}
		}
	}
	return objects
}
func object(t *testing.T, objects []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, o := range objects {
		if o["kind"] == kind && o["metadata"].(map[string]any)["name"] == name {
			return o
		}
	}
	t.Fatalf("missing %s %s", kind, name)
	return nil
}
func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestChartDefaults(t *testing.T) {
	t.Parallel()
	objects := rendered(t, nil)
	cm := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
	if cm["parallelism"] != "4" || !strings.Contains(cm["mainContainer"].(string), "drop: [ALL]") || !strings.Contains(cm["failedPodRestart"].(string), "maxRestarts: 1") {
		t.Fatal(cm)
	}
	if !strings.Contains(cm["artifactDrivers"].(string), "oberth-source-plugin") {
		t.Fatal(cm)
	}
	object(t, objects, "NetworkPolicy", "oberth-egress")
	deploy := object(t, objects, "Deployment", "oberth")
	if !strings.Contains(jsonOf(t, deploy), `"medium":"Memory"`) || !strings.Contains(jsonOf(t, deploy), `"type":"Recreate"`) {
		t.Fatal(deploy)
	}
	ns := object(t, objects, "Namespace", "oberth-runs")
	if ns["metadata"].(map[string]any)["labels"].(map[string]any)["pod-security.kubernetes.io/enforce"] != "baseline" {
		t.Fatal(ns)
	}
	config := object(t, objects, "ConfigMap", "oberth-server")["data"].(map[string]any)["server.json"].(string)
	if strings.Contains(config, `"witness"`) {
		t.Fatal("disabled witness emitted", config)
	}
	assertNativeDependencies(t, objects)
}
func TestChartExplicitChoices(t *testing.T) {
	t.Parallel()
	objects := rendered(t, map[string]any{"runs": map[string]any{"parallel": 0}, "policies": map[string]any{"floor": false, "egress": false}})
	cm := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
	if cm["parallelism"] != "0" || cm["mainContainer"] != nil {
		t.Fatal(cm)
	}
	for _, o := range objects {
		if o["kind"] == "NetworkPolicy" && o["metadata"].(map[string]any)["name"] == "oberth-egress" {
			t.Fatal("egress off lost")
		}
	}
}
func TestSourceScratchFloor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		quantity string
		ok       bool
	}{{"2Gi", true}, {"1856Mi", true}, {"1855Mi", false}, {"1.8125Gi", true}, {"1946157056", true}, {"1946157055", false}, {"1946157055.9", true}, {"1946157056e0", true}, {"1856wat", false}, {"", false}} {
		t.Run(tc.quantity, func(t *testing.T) {
			ch, err := loader.Load(chartPath())
			if err != nil {
				t.Fatal(err)
			}
			_, err = installer.Render(ch, map[string]any{"storage": map[string]any{"tmp": tc.quantity}}, "oberth")
			if (err == nil) != tc.ok {
				t.Fatal(tc, err)
			}
			if tc.ok {
				assertScratchQuantity(t, tc.quantity)
			}
		})
	}
}
func TestChartSchemaAndLiteralValues(t *testing.T) {
	t.Parallel()
	for _, values := range []map[string]any{{"unknown": 1}, {"storage": map[string]any{"unknown": true}}, {"runs": map[string]any{"parallel": -1}}} {
		ch, err := loader.Load(chartPath())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = installer.Render(ch, values, "oberth"); err == nil {
			t.Fatal("invalid values accepted", values)
		}
	}
	objects := rendered(t, map[string]any{"database": map[string]any{"file": "/var/lib/oberth/{{ .Release.Name }}.db"}})
	if !strings.Contains(jsonOf(t, object(t, objects, "ConfigMap", "oberth-server")), "{{ .Release.Name }}") {
		t.Fatal("value executed as template")
	}
}
func TestChartPortContract(t *testing.T) {
	t.Parallel()
	t.Run("fake", func(t *testing.T) { contracttest.Chart(t, func() installer.ChartPort { return &fake.Chart{} }) })
	t.Run("library", func(t *testing.T) {
		contracttest.Chart(t, func() installer.ChartPort {
			return installer.ChartForTest(&action.Configuration{Releases: storage.Init(driver.NewMemory()), KubeClient: &kubefake.PrintingKubeClient{Out: io.Discard}, Capabilities: chartutil.DefaultCapabilities, Log: func(string, ...any) {}})
		})
	})
}

func TestServerAndPipelinePermissions(t *testing.T) {
	t.Parallel()
	objects := rendered(t, nil)
	role := object(t, objects, "Role", "oberth-server")
	b, err := yaml.Marshal(role)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("secrets")) || bytes.Contains(b, []byte("serviceaccounts/token")) {
		t.Fatal(string(b))
	}
	for _, rule := range role["rules"].([]any) {
		r := rule.(map[string]any)
		if strings.Contains(jsonOf(t, r["resources"]), `"workflows"`) && strings.Contains(jsonOf(t, r["verbs"]), `"delete"`) {
			t.Fatal("server can delete Workflows")
		}
	}
}

func assertNativeDependencies(t *testing.T, objects []map[string]any) {
	t.Helper()
	crds := 0
	for _, o := range objects {
		if o["kind"] == "CustomResourceDefinition" {
			crds++
		}
		if o["kind"] == "Job" {
			metadata := o["metadata"].(map[string]any)
			annotations, _ := metadata["annotations"].(map[string]any)
			if metadata["name"] != "oberth-release-finalizers" || annotations["helm.sh/hook"] != "pre-delete" {
				t.Fatal("unexpected install process job")
			}
		}
	}
	if crds < 7 {
		t.Fatal("native CRDs missing", crds)
	}
	object(t, objects, "StatefulSet", "oberth-store")
}

func assertScratchQuantity(t *testing.T, quantity string) {
	t.Helper()
	dep := object(t, rendered(t, map[string]any{"storage": map[string]any{"tmp": quantity}}), "Deployment", "oberth")
	volumes := dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["volumes"].([]any)
	for _, value := range volumes {
		volume := value.(map[string]any)
		if volume["name"] == "tmp" {
			if volume["emptyDir"].(map[string]any)["sizeLimit"] != quantity {
				t.Fatal("scratch render differs from validation", volume)
			}
			return
		}
	}
	t.Fatal("scratch volume missing")
}
