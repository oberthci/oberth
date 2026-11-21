package publicerr

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestPermanentRequiresEveryPublicCauseToBeFixable(t *testing.T) {
	private := errors.New("private validation detail")
	input := New(InvalidInput, "input is invalid", "Correct the input.", private)
	denied := New(Forbidden, "access is denied", "Request access.", nil)
	absent := New(NotFound, "record is absent", "Choose an existing record.", nil)
	outage := New(Unavailable, "store is unavailable", "Restore store access.", nil)
	var nilPublic *Error
	var nilWrapped *nilWrapper
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false}, {"typed nil", nilPublic, false}, {"typed nil wrapper", nilWrapped, false},
		{"private leaf", private, false}, {"private wrapper", fmt.Errorf("wrapped: %w", private), false},
		{"input with private detail", input, true}, {"missing", absent, true}, {"forbidden", denied, true},
		{"joined refusals", errors.Join(input, denied, absent), true},
		{"wrapped refusals", fmt.Errorf("wrapped: %w", errors.Join(input, absent)), true},
		{"outage", outage, false}, {"outage after refusal", errors.Join(input, outage), false},
		{"outage before refusal", errors.Join(outage, input), false},
		{"unknown sibling", errors.Join(input, private), false},
		{"canceled sibling", errors.Join(input, context.Canceled), false},
		{"deadline sibling", errors.Join(input, context.DeadlineExceeded), false},
		{"nested outage", New(InvalidInput, "input is invalid", "Correct the input.", outage), false},
		{"wrapped typed nil", fmt.Errorf("wrapped: %w", nilPublic), false},
		{"lookalike", lookalikeError{}, false},
	}
	for _, code := range []Code{InvalidTransition, Ambiguous, Conflict, SchemaRefused, Invariant} {
		cases = append(cases, struct {
			name string
			err  error
			want bool
		}{string(code), New(code, "operation was refused", "Check the operation.", nil), false})
	}
	for _, row := range cases {
		t.Run(row.name, func(t *testing.T) {
			if got := Permanent(row.err); got != row.want {
				t.Fatalf("Permanent=%v, want %v", got, row.want)
			}
		})
	}
}
