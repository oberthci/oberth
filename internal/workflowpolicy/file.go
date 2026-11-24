package workflowpolicy

import (
	"strings"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

const (
	BuildFile    = ".oberth/build.yaml"
	ReleaseFile  = ".oberth/release.yaml"
	MaxFileBytes = 1024
)

// InvalidError names the policy field that cannot establish an exact selection.
type InvalidError struct {
	Path   string
	reason string
	detail string
}

func (e *InvalidError) Error() string {
	return e.Path + ": " + e.Reason() + ". " + e.Remedy() + " " + e.detail
}

func (e *InvalidError) Reason() string {
	if e.reason == "" {
		return "policy selection is incomplete"
	}
	return e.reason
}

func (e *InvalidError) Remedy() string {
	switch e.Path {
	case "manual_release":
		return "Ask an administrator to release this workflow through its tag route."
	case "manual_ref":
		return "Push a branch with oberth.run=1 and oberth.file=<path>."
	case "file":
		return "Use an exact clean repository-relative workflow file."
	case "ref":
		return "Select a bare tag with its durable tag kind."
	case "repo":
		return "Use the registered upstream/org/repository name."
	case "routes", "pattern":
		return "Configure bounded, distinct tag patterns that select one release file."
	case "step":
		return "Use the exact native step selector from the plan."
	case "path":
		return "Use an exact public credential path without wildcards or traversal."
	case "audience":
		return "Use the exact audience approved by the grant."
	}
	return "Correct the named policy entry and retry."
}

// ValidateFile accepts only an exact clean repository-relative path. No
// wildcard, normalization, or inferred workflow is allowed in a grant key.
func ValidateFile(file string) error {
	if len(file) > MaxFileBytes || sourcecontract.ValidateFilePath(file) != nil || !policyAlphabet(file) {
		return &InvalidError{Path: "file", reason: "workflow file is invalid"}
	}
	return nil
}

func policyAlphabet(value string) bool {
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._/-", c) {
			continue
		}
		return false
	}
	return true
}
