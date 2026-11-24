package installer_test

import "testing"

func TestChosenRunCapDoesNotChangeServerInputs(t *testing.T) {
	baseline := rendered(t, nil)
	server := object(t, baseline, "Deployment", "oberth")["spec"].(map[string]any)["template"]
	config := object(t, baseline, "ConfigMap", "oberth-server")["data"]
	for _, cap := range []int{0, 1, 4, 6} {
		objects := rendered(t, map[string]any{"runs": map[string]any{"parallel": cap}, "statusChosen": []any{"parallel runs"}})
		if jsonOf(t, object(t, objects, "Deployment", "oberth")["spec"].(map[string]any)["template"]) != jsonOf(t, server) || jsonOf(t, object(t, objects, "ConfigMap", "oberth-server")["data"]) != jsonOf(t, config) {
			t.Fatal("chosen run cap changes server inputs", cap)
		}
		controller := object(t, objects, "ConfigMap", "oberth-controller-config")
		if controller["metadata"].(map[string]any)["annotations"].(map[string]any)["oberth.ci/parallel-origin"] != "chosen" {
			t.Fatal("explicit run cap origin lost")
		}
	}
}
