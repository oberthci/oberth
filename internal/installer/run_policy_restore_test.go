package installer_test

import (
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestRunPolicyCanRemoveNamespaceShare(t *testing.T) {
	for _, namespaces := range []string{
		`{"oberth-runs":{"cpuMilli":9000,"memoryMi":49152,"pods":80,"ephemeralGi":256}}`,
		`{"children":null,"oberth-runs":{"cpuMilli":9000,"memoryMi":49152,"pods":80,"ephemeralGi":256}}`,
	} {
		t.Run(namespaces, func(t *testing.T) {
			f := setup(t)
			f.choices.Values = runPolicyFixture()
			apply(t, f, prepare(t, f))
			selected, err := installer.RunPolicyChoices(`{"namespaces":` + namespaces + `}`)
			if err != nil {
				t.Fatal(err)
			}
			f.choices = installer.Choices{Context: "test", Values: selected}
			apply(t, f, prepare(t, f))
			last := f.charts.Applies[len(f.charts.Applies)-1]
			policy := last.Values["runPolicy"].(map[string]any)
			allocations := policy["namespaces"].(map[string]any)
			if policy["enabled"] != true || len(allocations) != 1 || allocations["children"] != nil {
				t.Fatal("removed child allocation survived or policy disabled", policy)
			}
			for _, o := range rendered(t, last.Values) {
				if o["metadata"].(map[string]any)["namespace"] == "children" {
					t.Fatal("removed child policy still rendered", o)
				}
			}
		})
	}
}

func TestRunPolicyCanRestoreAbsentDefaultsWithoutChangingOtherChoices(t *testing.T) {
	for _, reset := range []string{
		`{"default":{"cpu":null},"defaultRequest":{"ephemeral-storage":null}}`,
		`{"default":{},"defaultRequest":{}}`,
		`{"default":null,"defaultRequest":null}`,
	} {
		t.Run(reset, func(t *testing.T) {
			f := setup(t)
			resources, err := installer.ResourceChoices(`{"server":{"requests":{"ephemeral-storage":"256Mi"}},"controller":{"requests":{"ephemeral-storage":"128Mi"}},"store":{"requests":{"ephemeral-storage":"64Mi"}},"executor":{"requests":{"cpu":"100m"}}}`)
			if err != nil {
				t.Fatal(err)
			}
			resources["runPolicy"] = runPolicyFixture()["runPolicy"]
			f.choices.Values = resources
			parallel := 7
			f.choices.Parallel = &parallel
			apply(t, f, prepare(t, f))
			before := f.charts.Applies[len(f.charts.Applies)-1].Values
			trial, err := installer.RunPolicyChoices(`{"default":{"cpu":"1"},"defaultRequest":{"ephemeral-storage":"2Gi"}}`)
			if err != nil {
				t.Fatal(err)
			}
			f.choices = installer.Choices{Context: "test", Values: trial}
			apply(t, f, prepare(t, f))
			selected, err := installer.RunPolicyChoices(reset)
			if err != nil {
				t.Fatal(err)
			}
			f.choices = installer.Choices{Context: "test", Values: selected}
			apply(t, f, prepare(t, f))
			last := f.charts.Applies[len(f.charts.Applies)-1]
			assertPolicyRestoreChoices(t, before, last.Values)
			f.choices = installer.Choices{Context: "test"}
			apply(t, f, prepare(t, f))
			if jsonOf(t, before["runPolicy"]) != jsonOf(t, f.charts.Applies[len(f.charts.Applies)-1].Values["runPolicy"]) {
				t.Fatal("restored policy changed on omitted upgrade")
			}
		})
	}
}

func assertPolicyRestoreChoices(t *testing.T, before, after map[string]any) {
	t.Helper()
	for _, name := range []string{"runPolicy", "resources", "argoNative", "storeNative", "runs"} {
		if jsonOf(t, before[name]) != jsonOf(t, after[name]) {
			t.Fatal("restore changed saved choice", name, after[name])
		}
	}
	beforeObjects, afterObjects := rendered(t, before), rendered(t, after)
	for _, component := range [][2]string{{"Deployment", "oberth"}, {"Deployment", installer.ControllerDeployment}, {"StatefulSet", "oberth-store"}, {"ConfigMap", "oberth-controller-config"}, {"LimitRange", "oberth-run-defaults"}} {
		if !reflect.DeepEqual(object(t, beforeObjects, component[0], component[1]), object(t, afterObjects, component[0], component[1])) {
			t.Fatal("policy restore changed native object", component)
		}
	}
}
