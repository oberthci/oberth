package gitport

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestTypedErrorsSurviveJoinedWrappers(t *testing.T) {
	absent := &AbsentError{Repository: "repo", Ref: "ref", Path: "file"}
	author := &AuthorRefusalError{Path: "file", Code: "invalid", Hint: "correct input"}
	unavailable := &UnavailableError{Op: "read", Cause: context.Canceled}
	cases := []struct {
		name   string
		err    error
		target func(error) bool
	}{
		{"absent", absent, func(err error) bool {
			var target *AbsentError
			return errors.As(err, &target) && target == absent
		}},
		{"author refusal", author, func(err error) bool {
			var target *AuthorRefusalError
			return errors.As(err, &target) && target == author
		}},
		{"unavailable", unavailable, func(err error) bool {
			var target *UnavailableError
			return errors.As(err, &target) && target == unavailable && errors.Is(err, context.Canceled)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := fmt.Errorf("operation: %w", errors.Join(errors.New("another cause"), tc.err))
			if !tc.target(wrapped) {
				t.Fatal("typed error or its evidence was lost")
			}
			if tc.target(errors.New("unrelated")) {
				t.Fatal("unrelated error matched")
			}
		})
	}
}

func TestUnavailableDoesNotImplyAbsenceOrAuthorRefusal(t *testing.T) {
	err := &UnavailableError{Op: "read", Cause: context.DeadlineExceeded}
	var absent *AbsentError
	var author *AuthorRefusalError
	if errors.As(err, &absent) || errors.As(err, &author) {
		t.Fatal("unavailable operation was treated as a permanent input failure")
	}
}

func TestMixedCausesRemainVisible(t *testing.T) {
	err := errors.Join(&AbsentError{}, &AuthorRefusalError{}, &UnavailableError{Cause: context.Canceled})
	var absent *AbsentError
	var author *AuthorRefusalError
	var unavailable *UnavailableError
	if !errors.As(err, &absent) || !errors.As(err, &author) || !errors.As(err, &unavailable) || !errors.Is(err, context.Canceled) {
		t.Fatal("joined causes were lost")
	}
}
