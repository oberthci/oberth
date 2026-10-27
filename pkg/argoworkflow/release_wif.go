package argoworkflow

import k8svalidation "k8s.io/apimachinery/pkg/util/validation"

// ReleaseWIFRoleAnnotation requests one administrator-approved capability on
// a named release Pod leaf. It does not select an audience or service account.
const ReleaseWIFRoleAnnotation = "oberth.ci/release-wif-role"

// ValidReleaseWIFRole checks syntax only. The server must additionally require
// the role in the administrator's exact repository/template capability map.
func ValidReleaseWIFRole(role string) bool {
	return len(k8svalidation.IsDNS1123Label(role)) == 0
}
