package workflowpolicy

import (
	"path"
	"strings"
	"unicode"

	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/reponame"
)

// GrantKey combines the shared file/step tuple with a public credential path and
// an optional identity audience. Declaring this value does not prove approval.
type GrantKey struct {
	identityname.Key
	Path     string `json:"path"`
	Audience string `json:"audience"`
}

// Validate refuses incomplete grants. Secret grants omit Audience; identity
// grants supply an exact audience. Step "*" covers every step of that file.
func (k GrantKey) Validate() error {
	if _, err := reponame.ParseRepository(k.Repository); err != nil {
		return &InvalidError{Path: "repo", reason: "requires a canonical repository"}
	}
	if err := ValidateFile(k.File); err != nil {
		return err
	}
	if !exactStep(k.Step) {
		return &InvalidError{Path: "step", reason: "requires an exact step"}
	}
	if !ValidSecretPath(k.Path) {
		return &InvalidError{Path: "path", reason: "requires an exact public credential path"}
	}
	if strings.IndexFunc(k.Audience, audienceSpace) >= 0 {
		return &InvalidError{Path: "audience", reason: "requires an exact audience without whitespace or control characters"}
	}
	return nil
}

// ValidSecretPath is the shared syntax for a declared, granted and stored secret.
func ValidSecretPath(value string) bool {
	return value != "" && len(value) <= 1024 && policyAlphabet(value) &&
		!path.IsAbs(value) && path.Clean(value) == value && value != "." && !strings.Contains(value, "..")
}

// Native invocation selectors include structural brackets and loop parentheses.
// They are compared literally, never interpreted as filesystem paths or globs.
func exactStep(step string) bool {
	return step != "" && step != "?" && !strings.ContainsAny(step, "\x00\r\n")
}

func audienceSpace(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }

// Matches compares repository, file and step. Step "*" matches all steps only
// within the same repository and file; path/audience approval remains separate.
func (k GrantKey) Matches(key identityname.Key) bool {
	return k.Repository != "" && k.File != "" && key.Step != "" &&
		k.Repository == key.Repository && k.File == key.File && (k.Step == "*" || k.Step == key.Step)
}
