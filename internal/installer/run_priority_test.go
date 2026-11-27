package installer_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestRunClassesAreInstallationScopedAndNonpreempting(t *testing.T) {
	t.Parallel()
	values, err := installer.RunPriorityChoices(`{"release":30000,"promotion":20000,"qualification":10000,"lint":0}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, namespace := range []string{"oberth", "second"} {
		objects := renderedIn(t, values, namespace)
		assertRunClassCount(t, objects, 4)
		for tier, value := range map[string]float64{"release": 30000, "promotion": 20000, "qualification": 10000, "lint": 0} {
			priority := object(t, objects, "PriorityClass", "oberth-run-"+namespace+"-"+tier)
			if priority["value"] != value || priority["globalDefault"] != false || priority["preemptionPolicy"] != "Never" {
				t.Fatal("run class changed native order or enables preemption", priority)
			}
			meta := priority["metadata"].(map[string]any)
			if meta["annotations"].(map[string]any)["helm.sh/resource-policy"] != "keep" {
				t.Fatal("rollback can remove a class still used by admitted workflows")
			}
		}
	}
}

func assertRunClassCount(t *testing.T, objects []map[string]any, want int) {
	t.Helper()
	count := 0
	for _, resource := range objects {
		if resource["kind"] == "PriorityClass" && strings.HasPrefix(resource["metadata"].(map[string]any)["name"].(string), "oberth-run-") {
			count++
		}
	}
	if count != want {
		t.Fatal("run classes have more than one owner or lack an explicit choice", count, want)
	}
}
