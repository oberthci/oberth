package installer_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/chart/loader"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

func TestControllerConfigReloadRightsInOwnNamespace(t *testing.T) {
	t.Parallel()
	var deployment string
	for _, parallel := range []int{4, 2} {
		objects := renderedIn(t, map[string]any{"runs": map[string]any{"parallel": parallel}, "pipeline": map[string]any{"namespace": "jobs"}, "argoNative": map[string]any{"namespaceOverride": "frontdoor"}}, "frontdoor")
		assertControllerReloadRole(t, objects)
		config := object(t, objects, "ConfigMap", "oberth-controller-config")["data"].(map[string]any)
		want := "4"
		if parallel == 2 {
			want = "2"
		}
		if config["instanceID"] != "frontdoor" {
			t.Fatal("controller instance differs from server namespace", config)
		}
		if config["parallelism"] != want {
			t.Fatal("parallelism was not applied to controller data", config)
		}
		current := jsonOf(t, object(t, objects, "Deployment", "oberth-controller-workflow-controller"))
		if deployment != "" && deployment != current {
			t.Fatal("parallelism change restarts controller instead of native reload")
		}
		deployment = current
	}
}
func assertControllerReloadRole(t *testing.T, objects []map[string]any) {
	t.Helper()
	var role rbacv1.Role
	if err := yaml.Unmarshal([]byte(jsonOf(t, object(t, objects, "Role", "oberth-controller-config"))), &role); err != nil {
		t.Fatal(err)
	}
	want := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"oberth-controller-config"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"create"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{"workflow-controller", "workflow-controller-lease"}, Verbs: []string{"get", "watch", "update", "patch", "delete"}},
	}
	if role.Namespace != "frontdoor" || !reflect.DeepEqual(role.Rules, want) {
		t.Fatal("controller reload authority differs", role)
	}
	var binding rbacv1.RoleBinding
	if err := yaml.Unmarshal([]byte(jsonOf(t, object(t, objects, "RoleBinding", "oberth-controller-config"))), &binding); err != nil {
		t.Fatal(err)
	}
	if binding.Namespace != "frontdoor" || binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}) || !reflect.DeepEqual(binding.Subjects, []rbacv1.Subject{{Kind: "ServiceAccount", Name: "oberth-controller", Namespace: "frontdoor"}}) {
		t.Fatal("reload binding escaped controller namespace", binding)
	}
}

func TestRenderReloadsArchiveForEachOperation(t *testing.T) {
	t.Parallel()
	ch, err := loader.Load(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		files, e := installer.Render(ch, nil, "oberth")
		if e != nil {
			t.Fatal(e)
		}
		controller := false
		for name, raw := range files {
			if strings.Contains(name, "argoNative") && strings.Contains(raw, "kind: Deployment") {
				controller = true
			}
			if strings.Contains(name, "templates/tests/") {
				t.Fatal("upstream test hook retained", name)
			}
		}
		if !controller {
			t.Fatal("bundled controller missing", controller)
		}
	}
	if len(ch.Dependencies()) != 2 {
		t.Fatal("source archive dependencies were mutated")
	}
}
