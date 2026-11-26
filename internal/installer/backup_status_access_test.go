package installer_test

import (
	"encoding/json"
	"reflect"
	"testing"

	rbac "k8s.io/api/rbac/v1"
)

func TestBackupStatusRoleOnlyReadsTheNamedCronJob(t *testing.T) {
	objects := renderedIn(t, map[string]any{"pipeline": map[string]any{"namespace": "runs"}}, "server")
	var role rbac.Role
	var binding rbac.RoleBinding
	if err := json.Unmarshal([]byte(jsonOf(t, object(t, objects, "Role", "oberth-backup-status"))), &role); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(jsonOf(t, object(t, objects, "RoleBinding", "oberth-backup-status"))), &binding); err != nil {
		t.Fatal(err)
	}
	want := []rbac.PolicyRule{{APIGroups: []string{"batch"}, Resources: []string{"cronjobs"}, ResourceNames: []string{"oberth-backup"}, Verbs: []string{"get"}}}
	if role.Namespace != "server" || !reflect.DeepEqual(role.Rules, want) || binding.Namespace != "server" || binding.RoleRef.Name != role.Name || binding.RoleRef.Kind != "Role" || !reflect.DeepEqual(binding.Subjects, []rbac.Subject{{Kind: "ServiceAccount", Name: "oberth", Namespace: "server"}}) {
		t.Fatal("backup status reader lacks its named read or grants wider access")
	}
}
