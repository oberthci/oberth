package installer_test

import (
	"encoding/json"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"slices"
)

func TestServerRoleOmitsUnusedPodAndAccountWrites(t *testing.T) {
	t.Parallel()
	role := object(t, rendered(t, nil), "Role", "oberth-server")
	var actual rbacv1.Role
	if err := json.Unmarshal([]byte(jsonOf(t, role)), &actual); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ resource, verb string }{{"pods", "patch"}, {"pods", "delete"}, {"pods/exec", "create"}, {"serviceaccounts", "delete"}} {
		t.Run(tc.resource+"/"+tc.verb, func(t *testing.T) {
			for _, rule := range actual.Rules {
				if (slices.Contains(rule.Resources, tc.resource) || slices.Contains(rule.Resources, "*")) && (slices.Contains(rule.Verbs, tc.verb) || slices.Contains(rule.Verbs, "*")) {
					t.Fatal("unused write allowed", tc)
				}
			}
		})
	}
}
