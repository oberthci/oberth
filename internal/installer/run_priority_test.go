package installer_test

import "testing"

func TestRunClassesAreInstallationScopedAndNonpreempting(t *testing.T) {
	t.Parallel()
	for _, namespace := range []string{"oberth", "second"} {
		objects := renderedIn(t, nil, namespace)
		for tier, value := range map[string]float64{"release": 30000, "promotion": 20000, "qualification": 10000, "lint": 0} {
			priority := object(t, objects, "PriorityClass", "oberth-run-"+tier+"-"+namespace)
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
