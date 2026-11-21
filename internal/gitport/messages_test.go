package gitport

import (
	"errors"
	"strings"
	"testing"
)

func TestRefRefusalsNameTheFailedPart(t *testing.T) {
	id := strings.Repeat("a", 40)
	cases := []struct {
		name, detail string
		ref          Ref
	}{
		{"name", "revision syntax", Ref{Name: "refs/heads/a..b", ObjectSHA: id, CommitSHA: id}},
		{"object", "ref.object_sha", Ref{Name: "refs/heads/main", ObjectSHA: "short", CommitSHA: id}},
		{"commit", "ref.commit_sha", Ref{Name: "refs/heads/main", ObjectSHA: id, CommitSHA: "UPPER"}},
		{"missing object", "ref.object_sha", Ref{Name: "refs/heads/main", CommitSHA: id}},
		{"missing commit", "ref.commit_sha", Ref{Name: "refs/heads/main", ObjectSHA: id}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRef("origin/team/widget", tc.ref)
			var refusal *AuthorRefusalError
			if !errors.As(err, &refusal) {
				t.Fatalf("missing author refusal: %v", err)
			}
			checkDiagnostic(t, refusal)
			if !strings.Contains(refusal.Error(), tc.ref.Name) || !strings.Contains(refusal.Error(), tc.detail) {
				t.Fatalf("missing ref or failed part: %v", err)
			}
			if strings.ContainsAny(refusal.Reason(), "{}") || strings.Contains(refusal.Reason(), `""`) {
				t.Fatalf("struct or empty field in message: %v", err)
			}
		})
	}
}

func TestPromotionRefusalsDoNotPrintStructs(t *testing.T) {
	r := request()
	other := r
	other.Source.Repository = "origin/team/other"
	conflict := Promotion{Request: r, Result: Ref{Name: r.Target.Name}, Conflicts: []string{"file"}}
	for _, err := range []error{other.Validate(), conflict.Validate()} {
		var typed *AuthorRefusalError
		if !errors.As(err, &typed) {
			t.Fatal(err)
		}
		checkDiagnostic(t, typed)
		if err == nil || strings.ContainsAny(err.Error(), "{}") || strings.Contains(err.Error(), `""`) {
			t.Fatalf("unclear promotion refusal: %v", err)
		}
	}
}

func TestAbsentMessagesOmitUnspecifiedFields(t *testing.T) {
	cases := []struct{ value AbsentError }{
		{AbsentError{}}, {AbsentError{Repository: "origin/team/widget"}}, {AbsentError{Ref: "refs/heads/main"}}, {AbsentError{Path: "pipeline.yaml"}},
	}
	for _, tc := range cases {
		for _, value := range []string{tc.value.Repository, tc.value.Ref, tc.value.Path} {
			if !strings.Contains(tc.value.Error(), value) {
				t.Fatal("lookup detail lost", value)
			}
		}
		if tc.value.Reason() != "Git data was not found." {
			t.Fatal("lookup reason includes variable detail")
		}

	}
	for _, err := range []error{&AuthorRefusalError{}, &UnavailableError{}} {
		if strings.Contains(err.Error(), `""`) {
			t.Fatalf("empty field in message: %v", err)
		}
	}
}
