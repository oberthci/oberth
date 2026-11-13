package model

// SecretAccessKey is one exact repository, workflow-file, step and path approval.
type SecretAccessKey struct {
	Repo     string `json:"repo"`
	Workflow string `json:"workflow"`
	Step     string `json:"step"`
	//nolint:gosec // G117: This is a secret path identifier, never a secret value.
	Secret string `json:"secret"`
}
