package gitport

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func checkDiagnostic(t *testing.T, e interface {
	Reason() string
	Remedy() string
}) {
	t.Helper()
	for text, limit := range map[string]int{e.Reason(): 80, e.Remedy(): 120} {
		if err := publicerr.CheckMessage(text, limit); err != nil {
			t.Error(err, text)
		}
	}
}

func TestGitReasonCatalogueFitsAndRetainsLongContext(t *testing.T) {
	value := strings.Repeat("long context ", 100)
	for _, e := range []interface {
		Reason() string
		Remedy() string
	}{
		&AbsentError{}, &AbsentError{Repository: value, Ref: value, Path: value}, &UnavailableError{}, &UnavailableError{Op: value},
	} {
		checkDiagnostic(t, e)
	}
	for _, code := range []string{"", "invalid_object", "invalid_repository", "invalid_ref", "invalid_path", "missing_action", "missing_sources", "invalid_source", "conflicting_result", "wrong_target", "invalid_request", "duplicate_ref", "not_regular", "too_large"} {
		e := &AuthorRefusalError{Code: code, Path: value, Message: value}
		checkDiagnostic(t, e)
		if !strings.Contains(e.Error(), value) {
			t.Fatal("author context lost")
		}
	}
}

func TestRemainingValidationRemediesFit(t *testing.T) {
	r := request()
	ref := Ref{Name: "refs/heads/main", ObjectSHA: r.Source.Commit, CommitSHA: r.Source.Commit}
	for _, err := range []error{
		(Promotion{Request: r, Result: Ref{Name: "refs/heads/other"}}).Validate(),
		(Promotion{Request: r, Result: ref, Tree: "short"}).Validate(),
	} {
		assertAuthorRefusal(t, err, "")
	}
}
