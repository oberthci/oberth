package installer_test

import "testing"

func TestRunCapOnlyChangesControllerConfig(t *testing.T) {
	t.Parallel()
	baseline := rendered(t, nil)
	server := object(t, baseline, "Deployment", "oberth")["spec"].(map[string]any)["template"]
	config := object(t, baseline, "ConfigMap", "oberth-server")["data"]
	for _, cap := range []int{0, 1, 6} {
		objects := rendered(t, map[string]any{"runs": map[string]any{"parallel": cap}})
		got := object(t, objects, "Deployment", "oberth")["spec"].(map[string]any)["template"]
		if jsonOf(t, got) != jsonOf(t, server) {
			t.Fatal("changing run cap changes server Pod template", cap)
		}
		if jsonOf(t, object(t, objects, "ConfigMap", "oberth-server")["data"]) != jsonOf(t, config) {
			t.Fatal("changing run cap changes server config", cap)
		}
		controller := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
		if controller["parallelism"] != jsonOf(t, cap) {
			t.Fatal("controller run cap did not change", cap, controller)
		}
	}
	if _, present := serverJSON(t, baseline)["runs"]; present {
		t.Fatal("server config contains the controller run cap")
	}
}
