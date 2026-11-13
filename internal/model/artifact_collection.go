package model

import "strings"

// ArtifactCollectionDeadlineReason is retained in the existing run error field.
// It describes missing output evidence without changing the Argo verdict.
const ArtifactCollectionDeadlineReason = "artifact collection deadline exceeded"

// SplitArtifactCollectionDeadline separates the fixed output-evidence note from
// the Workflow error before failure classification. Other error text is intact.
func SplitArtifactCollectionDeadline(message string) (string, bool) {
	if message == ArtifactCollectionDeadlineReason {
		return "", true
	}
	if base, ok := strings.CutSuffix(message, "\n"+ArtifactCollectionDeadlineReason); ok {
		return base, true
	}
	return message, false
}

// WithArtifactCollectionDeadline appends the evidence note at most once.
func WithArtifactCollectionDeadline(message string) string {
	if _, present := SplitArtifactCollectionDeadline(message); present {
		return message
	}
	if message == "" {
		return ArtifactCollectionDeadlineReason
	}
	return message + "\n" + ArtifactCollectionDeadlineReason
}
