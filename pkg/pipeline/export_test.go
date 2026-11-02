package pipeline

// Scope names the authorization scope for errors and audit records.
func (path UpstreamSecretPath) Scope() string {
	if path.Repo == "" {
		return "org"
	}
	return "repo"
}
