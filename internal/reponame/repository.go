package reponame

import (
	"fmt"
	"strings"
)

// Repository is an exact upstream/organization/name selector.
type Repository struct{ Upstream, Organization, Name string }

// InvalidNameError identifies the refused value and its expected form.
type InvalidNameError struct {
	Component, Value string
	reserved         bool
}

func (e *InvalidNameError) Reason() string {
	if e.reserved {
		return "The upstream name is reserved for Oberth state."
	}
	switch e.Component {
	case "upstream":
		return "The upstream name is invalid."
	case "organization":
		return "The organization name is invalid."
	case "repository path":
		return "The repository path is invalid."
	case "forge user":
		return "The forge SSH user name is invalid."
	case "forge path":
		return "The forge URL path is invalid."
	}
	return "The repository name is invalid."
}

func (e *InvalidNameError) Remedy() string {
	if e.reserved {
		return "Choose a different upstream name."
	}
	switch e.Component {
	case "upstream":
		return "Use 1..53 lowercase letters, digits or hyphens, starting and ending with a letter or digit; for example origin."
	case "repository path":
		return "Use upstream/organization/name, such as origin/team/widget, without a leading slash or .git suffix."
	case "organization":
		return "Use a name like team with letters, digits, ._-; start alphanumeric and avoid consecutive dots."
	case "forge user":
		return "Use the forge's SSH user, like git: up to 32 letters, digits, ._-, starting alphanumeric."
	case "forge path":
		return "Use path parts like srv/team/widget.git: letters, digits, ._-, each starting alphanumeric."
	default:
		return "Use a name like widget with letters, digits, ._-; avoid consecutive dots and a .git suffix."
	}
}
func (e *InvalidNameError) Error() string {
	return e.Reason() + " " + e.Remedy() + fmt.Sprintf(" Value: %q.", e.Value)
}

// ParseRepository accepts the canonical three-part form without path aliases.
func ParseRepository(value string) (Repository, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 3 {
		return Repository{}, &InvalidNameError{Component: "repository path", Value: value}
	}
	if err := ValidateUpstreamName(parts[0]); err != nil {
		return Repository{}, err
	}
	if !validSegment(parts[1]) {
		return Repository{}, &InvalidNameError{Component: "organization", Value: parts[1]}
	}
	if err := validateName(parts[2]); err != nil {
		return Repository{}, err
	}
	return Repository{Upstream: parts[0], Organization: parts[1], Name: parts[2]}, nil
}

// ValidateSelector accepts the two repo argument forms: a bare name or a
// canonical path. Name resolution and ambiguity remain the consumer's decision.
func ValidateSelector(value string) error {
	if !strings.Contains(value, "/") {
		return validateName(value)
	}
	_, err := ParseRepository(value)
	return err
}

func validateName(value string) error {
	if !validSegment(value) || strings.HasSuffix(strings.ToLower(value), ".git") {
		return &InvalidNameError{Component: "repository name", Value: value}
	}
	return nil
}

// ValidateUpstreamName applies the registration rule used by derived identities.
func ValidateUpstreamName(value string) error {
	if !validUpstream(value) {
		return &InvalidNameError{Component: "upstream", Value: value}
	}
	switch value {
	case "global", "release", "data", "upstream", "sys", "metadata", "receive-outbox", "metrics":
		return &InvalidNameError{Component: "upstream", Value: value, reserved: true}
	}
	return nil
}

func validUpstream(value string) bool {
	if len(value) == 0 || len(value) > 53 {
		return false
	}
	if value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}
func validSegment(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.Contains(value, "..") {
		return false
	}
	if !alphanumeric(value[0]) {
		return false
	}
	for i := 1; i < len(value); i++ {
		if !alphanumeric(value[i]) && !strings.ContainsRune("._-", rune(value[i])) {
			return false
		}
	}
	return true
}
func alphanumeric(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
