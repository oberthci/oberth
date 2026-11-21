package client

import (
	"errors"
	"github.com/oberthci/oberth/internal/publicerr"
)

type unknownOutcome struct{ problem *publicerr.Error }

func (e *unknownOutcome) Error() string { return e.problem.Error() }
func (e *unknownOutcome) Unwrap() error { return e.problem }

// IsOutcomeUnknown distinguishes an unanswered mutation from a known refusal.
func IsOutcomeUnknown(err error) bool {
	var outcome *unknownOutcome
	return errors.As(err, &outcome)
}
