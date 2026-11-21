package argojobtest

import (
	rbac "k8s.io/api/rbac/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TaskResultAccess is the test oracle for task-result writes and the account-group binding.
func TaskResultAccess(namespace string) (rbac.Role, rbac.RoleBinding) {
	role := rbac.Role{ObjectMeta: meta.ObjectMeta{Name: "oberth-task-results", Namespace: namespace}, Rules: []rbac.PolicyRule{{APIGroups: []string{"argoproj.io"}, Resources: []string{"workflowtaskresults"}, Verbs: []string{"create", "patch"}}}}
	binding := rbac.RoleBinding{ObjectMeta: meta.ObjectMeta{Name: role.Name, Namespace: namespace}, RoleRef: rbac.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: role.Name}, Subjects: []rbac.Subject{{Kind: "Group", APIGroup: "rbac.authorization.k8s.io", Name: "system:serviceaccounts:" + namespace}}}
	return role, binding
}
