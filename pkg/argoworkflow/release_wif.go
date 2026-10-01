package argoworkflow

// ReleaseWIFRoleAnnotation requests one administrator-approved capability on
// a named release Pod leaf. It does not select an audience or service account.
const ReleaseWIFRoleAnnotation = "oberth.ci/release-wif-role"

func ValidReleaseWIFRole(role string) bool {
	switch role {
	case "image-writer", "image-reader", "chart-writer":
		return true
	default:
		return false
	}
}
