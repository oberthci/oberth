package api

import (
	"slices"
	"testing"
)

func TestGenericExecutionToolsReplaceRetiredTools(t *testing.T) {
	names := ToolNames()
	for _, name := range []string{"kubevirt_grant", "kubevirt_revoke", "kubevirt_grant_get", "kubevirt_grant_list", "run_kvm_preflight"} {
		if slices.Contains(names, name) {
			t.Errorf("retired execution tool remains: %s", name)
		}
	}
	for _, name := range []string{"workflow_capability_grant", "workflow_capability_revoke", "workflow_capability_get", "run_cancel"} {
		if !slices.Contains(names, name) {
			t.Errorf("required generic execution tool missing: %s", name)
		}
	}
}
