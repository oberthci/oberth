package gitport

import (
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestValidationUsesAuthorRefusals(t *testing.T) {
	r := request()
	id := r.Source.Commit
	good := Ref{Name: "refs/heads/main", ObjectSHA: id, CommitSHA: id}
	cases := []struct {
		name, bad string
		call      func() error
	}{
		{"target ref", "refs/heads/a..b", func() error { v := r; v.Target.Name = "refs/heads/a..b"; return v.Validate() }},
		{"target object", "short", func() error { v := r; v.Target = good; v.Target.ObjectSHA = "short"; return v.Validate() }},
		{"promotion repository", "../bad", func() error { v := r; v.Repository = "../bad"; return v.Validate() }},
		{"source repository", "../bad", func() error { v := r; v.Source.Repository = "../bad"; return v.Validate() }},
		{"source object", "short", func() error { v := r; v.Source.Commit = "short"; return v.Validate() }},
		{"file path", "../file", func() error { return (File{Path: "../file", Blob: id, Mode: 0100644}).Validate(r.Source) }},
		{"file blob", "short", func() error { return (File{Path: "file", Blob: "short", Mode: 0100644}).Validate(r.Source) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertAuthorRefusal(t, tc.call(), tc.bad) })
	}
}

func assertAuthorRefusal(t *testing.T, err error, bad string) {
	t.Helper()
	var author *AuthorRefusalError
	var internal *sourcecontract.InvalidError
	if !errors.As(err, &author) || errors.As(err, &internal) {
		t.Fatalf("wrong error taxonomy: %T %v", err, err)
	}
	checkDiagnostic(t, author)
	if !strings.Contains(author.Error(), bad) || !strings.Contains(author.Remedy(), "Use ") {
		t.Fatalf("missing bad value/example: %v", err)
	}
}
