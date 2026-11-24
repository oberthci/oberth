package installer_test

import "testing"

func TestControlPodsHaveInstallationPriority(t *testing.T) {
	t.Parallel()
	for _, namespace := range []string{"oberth", "second", "system"} {
		t.Run(namespace, func(t *testing.T) {
			objects := renderedIn(t, nil, namespace)
			name := "oberth-control-" + namespace
			priority := object(t, objects, "PriorityClass", name)
			if priority["value"] != float64(1000000) || priority["globalDefault"] != false || priority["preemptionPolicy"] != "Never" {
				t.Fatal("control priority must exceed ordinary CI without becoming a system priority", priority)
			}
			quota := object(t, objects, "ResourceQuota", "oberth-control-priority")
			spec := quota["spec"].(map[string]any)
			if spec["hard"].(map[string]any)["pods"] != "0" {
				t.Fatal("CI may use control priority", quota)
			}
			rule := spec["scopeSelector"].(map[string]any)["matchExpressions"].([]any)[0].(map[string]any)
			if rule["scopeName"] != "PriorityClass" || rule["operator"] != "In" || rule["values"].([]any)[0] != name {
				t.Fatal("control quota targets another priority", rule)
			}
			for _, tc := range []struct{ kind, name string }{
				{"Deployment", "oberth"},
				{"Deployment", "oberth-controller-workflow-controller"},
				{"StatefulSet", "oberth-store"},
			} {
				pod := object(t, objects, tc.kind, tc.name)["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
				if pod["priorityClassName"] != name {
					t.Fatal("control Pod priority differs", tc.name, pod["priorityClassName"], name)
				}
			}
		})
	}
}
