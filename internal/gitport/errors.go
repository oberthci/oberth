package gitport

import (
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
)

type AbsentError struct{ Repository, Ref, Path string }

func (e *AbsentError) Reason() string { return "Git data was not found." }
func (e *AbsentError) detail() string {
	var parts []string
	for _, field := range []struct{ name, value string }{{"repository", e.Repository}, {"ref", e.Ref}, {"file", e.Path}} {
		if field.value != "" {
			parts = append(parts, fmt.Sprintf("%s %q", field.name, field.value))
		}
	}
	return strings.Join(parts, ", ")
}

func (*AbsentError) Remedy() string {
	return "Refresh the repository and select an existing exact revision and file."
}
func (e *AbsentError) Error() string {
	return strings.TrimSpace(e.Reason() + " " + e.Remedy() + " " + e.detail())
}

type AuthorRefusalError struct{ Path, Code, Message, Hint string }

func (e *AuthorRefusalError) Reason() string {
	return authorRefusalReason(e.Code)
}
func authorRefusalReason(code string) string {
	switch code {
	case "invalid_object":
		return "A Git object ID is invalid."
	case "invalid_repository":
		return "The Git repository is invalid."
	case "invalid_ref":
		return "The Git ref is invalid."
	case "invalid_path":
		return "The Git file path is invalid."
	case "missing_action":
		return "The attributed action receipt is missing."
	case "missing_sources":
		return "The promotion has no source revisions."
	case "invalid_source":
		return "The promotion source is invalid."
	case "conflicting_result":
		return "The promotion has conflicts and a publishable result."
	case "wrong_target":
		return "The promotion result names the wrong target."
	}
	return "The Git input is invalid."
}
func (e *AuthorRefusalError) Remedy() string {
	if e.Hint != "" {
		return e.Hint
	}
	return "Correct the input using the detailed field guidance, then retry."
}
func (e *AuthorRefusalError) Error() string {
	text := strings.TrimSuffix(e.Reason(), ".") + ". " + e.Remedy()
	if e.Message != "" {
		text += " " + e.Message
	}
	if e.Path != "" {
		text += fmt.Sprintf(" %q.", e.Path)
	}
	return text
}

type UnavailableError struct {
	Op    string
	Cause error
}

func (e *UnavailableError) Reason() string {
	if e.Op == "forge" {
		return ForgeReason
	}
	return "The Git operation could not finish."
}

func (e *UnavailableError) Remedy() string {
	if e.Op == "forge" {
		return ForgeRemedy
	}
	return "Check repository access and retry when the server is available."
}
func (e *UnavailableError) Error() string {
	text := e.Reason() + " " + e.Remedy()
	if e.Op == "forge" {
		return text
	}
	if e.Op != "" {
		text += fmt.Sprintf(" Operation: %q.", e.Op)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", text, e.Cause)
	}
	return text
}
func (e *UnavailableError) Unwrap() error {
	if e.Op == "forge" {
		return publicerr.New(publicerr.Unavailable, e.Reason(), e.Remedy(), e.Cause)
	}
	return e.Cause
}

func (e *AbsentError) Unwrap() error {
	return publicerr.New(publicerr.NotFound, e.Reason(), e.Remedy(), nil)
}
func (e *AuthorRefusalError) Unwrap() error {
	return publicerr.New(publicerr.InvalidInput, e.Reason(), e.Remedy(), nil)
}
