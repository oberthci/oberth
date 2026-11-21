package cronsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestMessageCatalogueIsBoundedAndComplete(t *testing.T) {
	for id, message := range messages() {
		if err := publicerr.CheckMessage(message.reason, 40); err != nil {
			t.Fatal(message, err)
		}
		if err := publicerr.CheckMessage(message.remedy, 120); err != nil {
			t.Fatal(message, err)
		}
		_ = problem(diagnostic(id), nil)
		if message.reason == "" || message.remedy == "" {
			t.Fatal("missing message", id)
		}
	}
	for _, err := range []error{&ChildRefusal{}} {
		if code := publicerr.Classify(err).Code(); code == publicerr.Invariant {
			t.Fatal("typed failure lost public classification", err)
		}
	}
}
func TestWholeErrorTreeClassification(t *testing.T) {
	author := publicerr.New(publicerr.InvalidInput, "pipeline is invalid", "Correct it.", errors.New("native validation diagnostic"))
	if transientError(fmt.Errorf("wrapped: %w", author)) != nil {
		t.Fatal("proven author refusal retained")
	}
	for _, outage := range []error{context.Canceled, context.DeadlineExceeded, errors.New("unknown infrastructure"), &gitport.UnavailableError{}, problem(nativeFailure, nil)} {
		for _, tree := range []error{errors.Join(author, outage), errors.Join(outage, author), fmt.Errorf("wrapped: %w", errors.Join(author, outage))} {
			if transientError(tree) == nil {
				t.Fatal("mixed cause converged", tree)
			}
		}
	}
}
func TestFileAbsenceCannotHideOtherFailures(t *testing.T) {
	absent := &gitport.AbsentError{Path: ".oberth/schedule.yaml"}
	if !optionalFile(fmt.Errorf("wrapped: %w", absent), absent.Path) {
		t.Fatal("missing file did not remove declaration")
	}
	for _, other := range []error{&gitport.AuthorRefusalError{}, errors.New("IO failed"), context.Canceled} {
		if optionalFile(errors.Join(absent, other), absent.Path) {
			t.Fatal("mixed absence authorized deletion")
		}
	}
}

func TestTypedFailureNeverSerializesItsPrivateCause(t *testing.T) {
	private := errors.New("private dependency data")
	for _, failure := range []error{&ChildRefusal{cause: private}} {
		data, err := json.Marshal(failure)
		if err != nil || strings.Contains(string(data), private.Error()) || strings.Contains(failure.Error(), private.Error()) || !errors.Is(failure, private) {
			t.Fatal("typed error lost privacy or cause", string(data), err)
		}
	}
}
