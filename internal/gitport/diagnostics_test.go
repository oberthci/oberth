package gitport

import (
	"errors"
	"strings"
	"testing"
)

func TestErrorDiagnosticsRetainFactsAndCause(t *testing.T) {
	absent := &AbsentError{Repository: "origin/team/widget", Ref: "refs/heads/main", Path: "pipeline.yaml"}
	for _, fact := range []string{absent.Repository, absent.Ref, absent.Path} {
		if !strings.Contains(absent.Error(), fact) {
			t.Fatal("missing lookup fact")
		}
	}
	cause := errors.New("permission denied")
	unavailable := &UnavailableError{Op: "fetch", Cause: cause}
	if !strings.Contains(unavailable.Error(), "fetch") || !strings.Contains(unavailable.Error(), cause.Error()) || !errors.Is(unavailable, cause) {
		t.Fatal("operation or cause was lost")
	}
	if strings.Contains(unavailable.Reason(), cause.Error()) {
		t.Fatal("private cause entered public reason")
	}
	for _, diagnosis := range []interface {
		Reason() string
		Remedy() string
	}{&AbsentError{}, &AuthorRefusalError{}, &UnavailableError{}} {
		if diagnosis.Reason() == "" || diagnosis.Remedy() == "" {
			t.Fatal("empty public diagnostic")
		}
	}
}
