package gitssh

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestSessionChannelOpenFailureClassification(t *testing.T) {
	for _, reason := range []ssh.RejectionReason{ssh.ResourceShortage, ssh.ConnectionFailed, ssh.Prohibited, ssh.UnknownChannelType} {
		t.Run(reason.String(), func(t *testing.T) {
			err := sessionFailure(fmt.Errorf("open session: %w", &ssh.OpenChannelError{Reason: reason, Message: "private upstream detail"}), "open Git SSH session")
			temporary := reason == ssh.ResourceShortage || reason == ssh.ConnectionFailed
			if errors.Is(err, ErrTransportUnavailable) != temporary || errors.Is(err, ErrRefused) == temporary {
				t.Fatalf("channel reason %v misclassified: %v", reason, err)
			}
			wantExit, wantMessage := 126, RefusalMessage
			if temporary {
				wantExit, wantMessage = 255, TemporaryFailureMessage
			}
			if ExitCode(err) != wantExit || Diagnostic(err) != wantMessage {
				t.Fatalf("unsafe session diagnostic: exit=%d message=%s", ExitCode(err), Diagnostic(err))
			}
		})
	}
}
