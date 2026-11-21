package publicerr

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestTaxonomyAndPrivateCause(t *testing.T) {
	codes := []Code{NotFound, InvalidInput, InvalidTransition, Ambiguous, Conflict, Forbidden, Unavailable, SchemaRefused, Invariant}
	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			if !code.Valid() {
				t.Fatal(code)
			}
			cause := errors.New("private credential detail")
			value := New(code, "public reason", "public remedy", cause)
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "private") || strings.Contains(value.Error(), "private") {
				t.Fatal("cause leaked")
			}
			var typed *Error
			if !errors.Is(value, cause) || !errors.As(fmt.Errorf("wrap: %w", errors.Join(cause, value)), &typed) {
				t.Fatal("cause chain lost")
			}
		})
	}
	if len(codes) != 9 || Code("").Valid() || Code("other").Valid() {
		t.Fatal("taxonomy expanded")
	}
}
func TestClassifyNearestBoundaryAndEveryInvariant(t *testing.T) {
	unavailable := New(Unavailable, "dependency unavailable", "retry observation", errors.New("private"))
	forbidden := New(Forbidden, "outside repository scope", "use a repository in scope", New(NotFound, "record absent", "check the ID", nil))
	invariant := New(Invariant, "ownership mismatch", "inspect the run binding", nil)
	var absent *Error
	var absentWrapper *nilWrapper
	cases := []struct {
		name string
		err  error
		code Code
	}{
		{"pure", unavailable, Unavailable},
		{"wrapped", fmt.Errorf("private: %w", unavailable), Unavailable},
		{"outer relabel", forbidden, Forbidden},
		{"equal depth follows join order", errors.Join(unavailable, forbidden), Unavailable},
		{"reverse join order", errors.Join(forbidden, unavailable), Forbidden},
		{"nearest across branches", errors.Join(fmt.Errorf("wrap: %w", forbidden), unavailable), Unavailable},
		{"unknown sibling", errors.Join(unavailable, errors.New("private")), Invariant},
		{"invariant sibling", errors.Join(unavailable, invariant), Invariant},
		{"invariant under relabel", New(Forbidden, "hidden", "check scope", invariant), Invariant},
		{"joined under relabel", New(Unavailable, "retry", "wait", errors.Join(unavailable, invariant)), Invariant},
		{"unknown", errors.New("private"), Invariant},
		{"zero public", &Error{}, Invariant},
		{"typed nil", absent, Invariant},
		{"typed nil wrapper", absentWrapper, Invariant},
		{"wrapped typed nil", fmt.Errorf("wrapper: %w", absent), Invariant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err)
			for text, limit := range map[string]int{got.Reason(): 80, got.Remedy(): 120} {
				if err := CheckMessage(text, limit); err != nil {
					t.Fatal(err, text)
				}
			}
			if got.Code() != tc.code {
				t.Fatalf("code=%s want %s", got.Code(), tc.code)
			}
			if got.Reason() == "" || got.Remedy() == "" || strings.Contains(got.Error(), "private") {
				t.Fatal("invalid public diagnostic")
			}
			if !errors.Is(got, tc.err) {
				t.Fatal("original tree lost")
			}
		})
	}
	if Classify(nil) != nil {
		t.Fatal("nil became an error")
	}
}
func TestSameCodeKeepsEveryOffenderAndRemedy(t *testing.T) {
	title := New(InvalidInput, "title is empty", "provide a title", nil)
	body := New(InvalidInput, "body exceeds 64 KiB", "shorten the body", nil)
	for _, err := range []error{errors.Join(title, body, title), errors.Join(body, title), New(InvalidInput, "request is invalid", "correct the fields", errors.Join(body, title))} {
		got := Classify(err)
		for _, text := range []string{"title is empty", "body exceeds 64 KiB", "provide a title", "shorten the body"} {
			if !strings.Contains(got.Error(), text) {
				t.Fatal("lost refusal", text, got)
			}
		}
		if strings.Count(got.Error(), "title is empty") != 1 {
			t.Fatal("duplicate refusal")
		}
	}
	first := Classify(errors.Join(title, body))
	second := Classify(errors.Join(body, title))
	if first.Reason() != second.Reason() || first.Remedy() != second.Remedy() {
		t.Fatal("same-code order changes output")
	}
	if first.Reason() != "Several problems blocked this operation." {
		t.Fatal(first.Reason())
	}
}
func TestConstructorRejectsMissingDiagnostics(t *testing.T) {
	for _, args := range []struct {
		code           Code
		reason, remedy string
	}{{"", "reason", "remedy"}, {Conflict, "", "reload"}, {Conflict, "revision moved", ""}, {Conflict, " ", "reload"}} {
		t.Run(string(args.code)+args.reason+args.remedy, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("programming error accepted")
				}
			}()
			_ = New(args.code, args.reason, args.remedy, nil)
		})
	}
}
func TestZeroAndNilPublicErrorAreActionable(t *testing.T) {
	for _, value := range []*Error{nil, new(Error)} {
		if value.Code() != Invariant || value.Reason() == "" || value.Remedy() == "" {
			t.Fatal("missing diagnostics")
		}
		if value.Unwrap() != nil {
			t.Fatal("absent cause changed")
		}
		if strings.Contains(value.Remedy(), "correlated") {
			t.Fatal("remedy refers to missing reference")
		}
	}
	raw, err := json.Marshal(new(Error))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"remedy":""`) {
		t.Fatal("empty remedy serialized")
	}
}

type nilWrapper struct{ cause error }

func (*nilWrapper) Error() string   { return "private wrapper" }
func (e *nilWrapper) Unwrap() error { return e.cause }

type lookalikeError struct{}

func (lookalikeError) Error() string  { return "private lookalike" }
func (lookalikeError) Code() Code     { return Forbidden }
func (lookalikeError) Reason() string { return "forged public reason" }
func (lookalikeError) Remedy() string { return "forged public remedy" }
func TestOnlyConcretePublicErrorsAreClassified(t *testing.T) {
	for _, err := range []error{lookalikeError{}, fmt.Errorf("wrapped: %w", lookalikeError{})} {
		got := Classify(err)
		if got.Code() != Invariant || strings.Contains(got.Error(), "forged") {
			t.Fatal("lookalike became public", got)
		}
	}
}
func TestErrorTextPreservesReasonRemedyPairs(t *testing.T) {
	a := New(InvalidInput, "alpha is invalid", "use zeta", nil)
	b := New(InvalidInput, "beta is invalid", "use alpha", nil)
	got := Classify(errors.Join(b, a, a))
	if got.Error() != "invalid_input: alpha is invalid - use zeta; beta is invalid - use alpha" {
		t.Fatal(got.Error())
	}
	if !strings.Contains(got.Error(), "alpha is invalid - use zeta; beta is invalid - use alpha") {
		t.Fatal("remedies lost reason order", got.Remedy())
	}
}
