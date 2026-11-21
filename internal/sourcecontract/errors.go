package sourcecontract

import (
	"strings"

	"github.com/oberthci/oberth/internal/gitoid"
)

// InvalidError reports a refused source value at a public structural path.
type InvalidError struct{ path, reason string }

func (e *InvalidError) Reason() string {
	if e.reason == "" {
		return "Source value is invalid."
	}
	return strings.TrimSuffix(e.reason, ".") + "."
}
func (e *InvalidError) Path() string { return e.path }
func (*InvalidError) Remedy() string { return "Correct the named source field and submit again." }
func (e *InvalidError) Error() string {
	return "Source " + e.path + ": " + e.Reason() + " " + e.Remedy()
}
func invalid(path, reason string) error { return &InvalidError{path: path, reason: reason} }

func validDigest(value string) bool { return len(value) == 64 && gitoid.Valid(value) }
