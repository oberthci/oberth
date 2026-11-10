// Package pushretry defines the conservative retry policy for upstream Git
// publication. Hook rejection alone never identifies a transient outage.
package pushretry

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitssh"
)

const MaxAttempts = 5
const failurePrefix = "upstream publication infrastructure failure: "

func Failure(message string) string          { return failurePrefix + message }
func PublicationFailure(message string) bool { return strings.HasPrefix(message, failurePrefix) }

// Delay is the delay after a failed attempt, not before the initial push.
func Delay(attempt int) time.Duration {
	if attempt < 1 || attempt >= MaxAttempts {
		return 0
	}
	return (2 * time.Minute) << (attempt - 1)
}

func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !Transient(cause) {
				return false
			}
		}
		return true
	}
	message := strings.ToLower(err.Error())
	// Explicit policy, identity and concurrency refusals always win, even if
	// a wrapper also contains a server-error line. The git-ssh shim's own
	// refusal line is one of them.
	for _, text := range []string{strings.ToLower(gitssh.RefusalMessage), "non-fast-forward", "fetch first", "protected" + " branch", "protected tag", "permission denied", "authentication failed", "access denied", "not authorized", "not allowed", "repository not found"} {
		if strings.Contains(message, text) {
			return false
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, gitssh.ErrTransportUnavailable) {
		return true
	}
	// The git-ssh shim prints this fixed line to Git's stderr when the TCP
	// dial, handshake or session stream failed. Git then appends its generic
	// "Could not read from remote repository ... correct access rights"
	// advice, which says nothing about the cause; the shim's line does.
	if strings.Contains(message, strings.ToLower(gitssh.TemporaryFailureMessage)) {
		return true
	}
	for _, text := range []string{"internal server connection error", "internal server error", "service unavailable", "bad gateway", "gateway timeout", "connection reset", "connection timed out", "connection timeout", "i/o timeout", "operation timed out", "remote end hung up unexpectedly", "unexpected disconnect", "could not resolve host", "temporary failure in name resolution", "connection refused", "network is unreachable"} {
		if strings.Contains(message, text) {
			return true
		}
	}
	for _, code := range []string{"500", "502", "503", "504"} {
		if strings.Contains(message, "returned error: "+code) || strings.Contains(message, "http "+code) || strings.Contains(message, "http status "+code) {
			return true
		}
	}
	return false
}
