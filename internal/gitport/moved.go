package gitport

import (
	"fmt"
	"github.com/oberthci/oberth/internal/publicerr"
)

// RefMovedError distinguishes an accepted local change from upstream movement.
// Location is local or upstream; neither is a transport outage.
type RefMovedError struct {
	Repository, Ref, Location, Expected, Actual string
}

func (e *RefMovedError) Reason() string {
	if e.Location == "local" {
		return "local ref moved"
	}
	return "upstream ref moved"
}
func (*RefMovedError) Remedy() string { return "Read the current ref and prepare publication again." }
func (e *RefMovedError) Error() string {
	return fmt.Sprintf("%s. %s Ref %q: expected %q, found %q.", e.Reason(), e.Remedy(), e.Ref, e.Expected, e.Actual)
}
func (e *RefMovedError) Unwrap() error {
	return publicerr.New(publicerr.Conflict, e.Reason(), e.Remedy(), nil)
}

type AlreadyExistsError struct{ Repository, Ref, ObjectSHA string }

func (*AlreadyExistsError) Reason() string  { return "upstream tag already exists" }
func (*AlreadyExistsError) Remedy() string  { return "Choose a new tag name." }
func (e *AlreadyExistsError) Error() string { return e.Reason() + ". " + e.Remedy() + " " + e.Ref }
func (e *AlreadyExistsError) Unwrap() error {
	return publicerr.New(publicerr.InvalidInput, e.Reason(), e.Remedy(), nil)
}
