package installer_test

import (
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestBackupSigningSecretIsOutsideControllerAndRunPermissions(t *testing.T) {
	objects := rendered(t, nil)
	for _, binding := range objects {
		kind, _ := binding["kind"].(string)
		if kind != "RoleBinding" && kind != "ClusterRoleBinding" {
			continue
		}
		if kind == "RoleBinding" && backupObjectNamespace(binding) != installer.Defaults().Namespace {
			continue
		}
		ref := binding["roleRef"].(map[string]any)
		if ref["kind"] == "ClusterRole" && ref["name"] == "system:auth-delegator" {
			continue
		}
		role := backupBoundRole(t, objects, binding)
		for _, value := range role["rules"].([]any) {
			if readsSigningSecret(value.(map[string]any)) {
				t.Fatal("another installed account can read the private backup signing Secret", ref["name"])
			}
		}
	}
}

func readsSigningSecret(rule map[string]any) bool {
	if !backupRuleIncludes(rule, "apiGroups", "", "*") || !backupRuleIncludes(rule, "resources", "secrets", "*") || !backupRuleIncludes(rule, "verbs", "get", "list", "watch", "*") {
		return false
	}
	names, ok := rule["resourceNames"].([]any)
	return !ok || len(names) == 0 || backupRuleIncludes(rule, "resourceNames", installer.BackupSigningSecret, "*")
}

func backupObjectNamespace(object map[string]any) string {
	meta := object["metadata"].(map[string]any)
	if namespace, _ := meta["namespace"].(string); namespace != "" {
		return namespace
	}
	return installer.Defaults().Namespace
}

func backupBoundRole(t *testing.T, objects []map[string]any, binding map[string]any) map[string]any {
	t.Helper()
	ref := binding["roleRef"].(map[string]any)
	for _, role := range objects {
		meta := role["metadata"].(map[string]any)
		if role["kind"] == ref["kind"] && meta["name"] == ref["name"] && (ref["kind"] == "ClusterRole" || backupObjectNamespace(role) == backupObjectNamespace(binding)) {
			return role
		}
	}
	t.Fatal("installed binding refers to an unreviewed role", ref["name"])
	return nil
}

func backupRuleIncludes(rule map[string]any, field string, candidates ...string) bool {
	values, _ := rule[field].([]any)
	for _, value := range values {
		if text, ok := value.(string); ok && slices.Contains(candidates, text) {
			return true
		}
	}
	return false
}
