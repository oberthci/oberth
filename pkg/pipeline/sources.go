package pipeline

// PinnedSource identifies public provenance, never upstream credentials.
type PinnedSource struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Path       string `json:"path"`
}
