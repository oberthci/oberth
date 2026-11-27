package installer_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/chart/loader"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

func TestRunPolicyIsOptInAndAggregatesAllNamespaces(t *testing.T) {
	for _, o := range rendered(t, nil) {
		if o["kind"] == "LimitRange" || strings.Contains(jsonOf(t, o["metadata"]), "oberth-run-capacity") {
			t.Fatal("default installation narrows native resources", o)
		}
	}
	values := runPolicyFixture()
	objects := rendered(t, values)
	for _, namespace := range []string{"oberth-runs", "children"} {
		var quota corev1.ResourceQuota
		var defaults corev1.LimitRange
		for _, o := range objects {
			if o["metadata"].(map[string]any)["namespace"] != namespace {
				continue
			}
			if o["metadata"].(map[string]any)["name"] == "oberth-run-capacity" {
				decodeObject(t, o, &quota)
			}
			if o["kind"] == "LimitRange" {
				decodeObject(t, o, &defaults)
			}
		}
		if len(quota.Spec.Hard) != 6 || quota.Spec.ScopeSelector != nil || len(quota.Spec.Scopes) != 0 || quota.Spec.Hard.Pods().Cmp(resource.MustParse("40")) != 0 {
			t.Fatal("quota omitted a dimension or allowed classes to escape", quota)
		}
		if len(defaults.Spec.Limits) != 1 || defaults.Spec.Limits[0].Type != corev1.LimitTypeContainer || !defaults.Spec.Limits[0].Default.Cpu().IsZero() {
			t.Fatal("native container defaults differ", defaults)
		}
	}
}

func TestRunPolicyRejectsDuplicatedEnvelopeAndUncoveredPipeline(t *testing.T) {
	ch, err := loader.Load(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, dimension := range []string{"cpuMilli", "memoryMi", "pods", "ephemeralGi"} {
		values := runPolicyFixture()
		allocations := values["runPolicy"].(map[string]any)["namespaces"].(map[string]any)
		allocations["children"].(map[string]any)[dimension] = 1 + allocations["children"].(map[string]any)[dimension].(int)
		if _, err := installer.Render(ch, values, "oberth"); err == nil || !strings.Contains(err.Error(), "Aggregate run quotas") {
			t.Fatal("aggregate overflow passed render", dimension, err)
		}
	}
	values := runPolicyFixture()
	delete(values["runPolicy"].(map[string]any)["namespaces"].(map[string]any), "oberth-runs")
	if _, err := installer.Render(ch, values, "oberth"); err == nil || !strings.Contains(err.Error(), "pipeline namespace") {
		t.Fatal("uncovered pipeline passed render", err)
	}
}

func TestExecutorResourcesReachNativeConfigWithoutChangingControls(t *testing.T) {
	values, err := installer.ResourceChoices(`{"executor":{"requests":{"cpu":"100m","memory":"128Mi","ephemeral-storage":"128Mi"},"limits":{"memory":"512Mi"}}}`)
	if err != nil {
		t.Fatal(err)
	}
	objects := rendered(t, values)
	var executor struct {
		Resources corev1.ResourceRequirements `json:"resources"`
	}
	data := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
	if err := yaml.Unmarshal([]byte(data["executor"].(string)), &executor); err != nil {
		t.Fatal(err)
	}
	if executor.Resources.Requests.Cpu().Cmp(resource.MustParse("100m")) != 0 || executor.Resources.Limits.Memory().Cmp(resource.MustParse("512Mi")) != 0 {
		t.Fatal("executor resources dropped", executor)
	}
	before := rendered(t, nil)
	for _, component := range [][2]string{{"Deployment", "oberth"}, {"Deployment", installer.ControllerDeployment}, {"StatefulSet", "oberth-store"}} {
		if !reflect.DeepEqual(object(t, before, component[0], component[1]), object(t, objects, component[0], component[1])) {
			t.Fatal("executor-only settings restarted a control", component)
		}
	}
}

func runPolicyFixture() map[string]any {
	return map[string]any{"runPolicy": map[string]any{"enabled": true, "namespaces": map[string]any{
		"oberth-runs": map[string]any{"cpuMilli": 4500, "memoryMi": 24576, "pods": 40, "ephemeralGi": 128},
		"children":    map[string]any{"cpuMilli": 4500, "memoryMi": 24576, "pods": 40, "ephemeralGi": 128},
	}}}
}
