package admissioncontract

// ReleaseFacts contains only server-verified admission facts. The admitted
// Workflow parameters retain them durably; no authored document supplies them.
type ReleaseFacts struct {
	Version, TestedCommit, DefaultCommit, BuildStatus string
}

func (f ReleaseFacts) Parameters() map[string]string {
	return map[string]string{
		"version":                f.Version,
		"release-tested-commit":  f.TestedCommit,
		"release-default-commit": f.DefaultCommit,
		"release-build-status":   f.BuildStatus,
	}
}

// IsReleaseParameter identifies workflow arguments and global outputs owned by admission.
func IsReleaseParameter(name string) bool {
	_, ok := (ReleaseFacts{}).Parameters()[name]
	return ok
}
