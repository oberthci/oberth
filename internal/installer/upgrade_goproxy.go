package installer

import (
	"fmt"
	"io"
)

// dropStoredGoProxy retires only the Oberth-owned proxy configuration. All
// other stored values still pass through the target chart's closed schema.
func dropStoredGoProxy(values map[string]any, output io.Writer) bool {
	argo, ok := values["argo"].(map[string]any)
	if !ok {
		return false
	}
	if _, present := argo["goProxy"]; !present {
		return false
	}
	delete(argo, "goProxy")
	if output != nil {
		_, _ = fmt.Fprintln(output, "Upgrade: removed stored argo.goProxy configuration; repositories must configure their own module access.")
	}
	return true
}

// Writes cannot reuse installed proxy values after its chart/backend removal.
// Read-only inspection reports this remedy without failing the installed read.
func refuseRemovedReleaseValues(installed map[string]any, command string) error {
	argo, _ := installed["argo"].(map[string]any)
	if _, present := argo["goProxy"]; !present {
		return nil
	}
	return &ErrSchemaValidation{Detail: fmt.Sprintf("the installed release values set chart values that no longer exist: argo.goProxy; %s changed nothing. Run oberth upgrade first: it automatically drops argo.goProxy and replaces the installed chart's defaults", command)}
}
