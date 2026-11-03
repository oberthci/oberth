package gitcache

import (
	"errors"
	"testing"
)

func TestPolicyRefreshRefusalDiagnosticsNeverRetry(t *testing.T) {
	for _, message := range []string{
		"oberth git-ssh: host key, identity or configuration refused",
		"oberth git-ssh: host key, identity or configuration refused: connection reset",
		"oberth git-ssh: transport refused or upstream Git service failed",
	} {
		if transientPolicyTransport(errors.New(message)) {
			t.Errorf("permanent or ambiguous refusal retried: %s", message)
		}
	}
}
