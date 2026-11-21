package gitcache

import (
	"errors"

	"github.com/oberthci/oberth/internal/gitport"
)

// Absence authorizes omission only when every leaf is absent. Mixed I/O errors
// remain unavailable, even if a joined sibling happens to be absent.
func isAbsent(err error) bool {
	if err == nil {
		return false
	}
	var unavailable *gitport.UnavailableError
	if errors.As(err, &unavailable) {
		return false
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		children := many.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !isAbsent(child) {
				return false
			}
		}
		return true
	}

	var absent *gitport.AbsentError
	if errors.As(err, &absent) && errors.Is(absent, err) {
		return true
	}
	if one, ok := err.(interface{ Unwrap() error }); ok {
		return isAbsent(one.Unwrap())
	}
	return false
}
