package gitssh

import (
	"errors"
	"strings"

	"github.com/oberthci/oberth/internal/publicerr"
	"golang.org/x/crypto/ssh"
)

// submissionFailure is evidence from the installed transport, never inferred
// from Git's exit status or arbitrary stderr. Underlying forge classes survive.
type submissionFailure struct {
	cause     error
	submitted bool
}

func (e *submissionFailure) Error() string { return e.cause.Error() }
func (e *submissionFailure) Unwrap() error { return e.cause }

func pushBoundary(command string, submitted bool, err error) error {
	var boundary *submissionFailure
	var remote *ssh.ExitError
	if err == nil || !strings.HasPrefix(command, "git-receive-pack") || errors.As(err, &remote) || errors.As(err, &boundary) {
		return err
	}
	return &submissionFailure{cause: err, submitted: submitted}
}

const PushLocalReason = "Local transport failed"
const PushLocalRefusal = "Push could not start its connection."
const PushLocalRemedy = "Fix the reported local SSH command or key path, then run the push again."
const PushUnknownReason = "Push outcome unknown"
const PushUnknownRefusal = "The connection ended before the push result arrived."
const PushUnknownRemedy = "Check the ref and run history before deciding whether to push again."

// PushDiagnostic scopes caller wording to the client shim. Cache forge readers
// continue to classify Run's underlying transport and refusal reasons.
func PushDiagnostic(err error) error {
	var boundary *submissionFailure
	if !errors.As(err, &boundary) {
		return err
	}
	if !boundary.submitted && RefusalFailure(err.Error()) {
		return err
	}
	refusal, remedy := PushLocalRefusal, PushLocalRemedy
	if boundary.submitted {
		refusal, remedy = PushUnknownRefusal, PushUnknownRemedy
	}
	// This replaces the transport wording at the client boundary. Keeping its
	// public error as a child would merge two remedies during classification.
	// Forge readers still receive Run's original error tree.
	return publicerr.New(publicerr.Unavailable, refusal, remedy, nil)
}
