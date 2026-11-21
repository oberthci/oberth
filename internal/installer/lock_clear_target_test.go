package installer_test

import (
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"sigs.k8s.io/yaml"
)

// Lock status prints `kubectl -n <argo_namespace> rollout restart
// deployment/<installer.ControllerDeployment>` for a stale unit. Every
// installation runs the managed controller: the chart's controller dependency
// has no condition, and it renders that Deployment in the namespace the server
// config names, also when the controller namespace is overridden.
func TestLockClearCommandNamesTheManagedController(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../charts/oberth/Chart.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Dependencies []struct{ Alias, Condition string } `json:"dependencies"`
	}
	if err = yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	managed := false
	for _, d := range chart.Dependencies {
		managed = managed || d.Alias == "argoNative" && d.Condition == ""
	}
	if !managed {
		t.Fatal("the workflow controller can be left out; the clear command needs a managed controller", chart.Dependencies)
	}
	for _, values := range []map[string]any{nil, {"argoNative": map[string]any{"namespaceOverride": "controller-ns"}}} {
		objects := renderedIn(t, values, "frontdoor")
		deployment := object(t, objects, "Deployment", installer.ControllerDeployment)
		if namespace := deployment["metadata"].(map[string]any)["namespace"]; namespace != serverJSON(t, objects)["argo_namespace"] {
			t.Fatal("clear command would name another namespace than the controller's", namespace)
		}
	}
}
