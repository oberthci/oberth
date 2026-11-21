package installer_test

import (
	"testing"
)

func TestInstallerAlwaysBundlesControllerInItsNamespace(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.Namespace = "frontdoor"
	p := prepare(t, f)
	request := f.charts.Previews[len(f.charts.Previews)-1]
	decoded := renderedIn(t, request.Values, request.Namespace)
	controller := object(t, decoded, "Deployment", "oberth-controller-workflow-controller")
	if controller["metadata"].(map[string]any)["namespace"] != "frontdoor" {
		t.Fatal("controller escaped installation")
	}
	p.Close()
}
