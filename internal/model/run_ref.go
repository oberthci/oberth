package model

import (
	"errors"
	"regexp"
)

// Shared by receive admission, stored-run compilation and release tag matching.
const runRefCharacterClass = `[A-Za-z0-9._/+@=,-]`

var runRefPattern = regexp.MustCompile(`^[A-Za-z0-9]` + runRefCharacterClass + `*$`)

// ValidateRunRef restricts branch names, refs and tags to the run-fact alphabet.
// Keep the refusal independent of the untrusted value and identical at receive
// and Build, including for runs whose ref was stored before this validation.
func ValidateRunRef(value string) error {
	if !runRefPattern.MatchString(value) {
		return errors.New("ref name is invalid: start with a letter or digit; use only letters, digits and . _ / + @ = , -")
	}
	return nil
}

// ValidateScheduleName uses the same alphabet for the schedule workflow parameter.
func ValidateScheduleName(value string) error {
	if len(value) > 64 || !runRefPattern.MatchString(value) {
		return errors.New("schedule name is invalid: at most 64 bytes; start with a letter or digit; use only letters, digits and . _ / + @ = , -")
	}
	return nil
}
