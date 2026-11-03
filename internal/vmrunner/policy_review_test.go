package vmrunner

import (
	"errors"
	"strings"
	"testing"
)

func TestResolveRequirementPermanentReadsFailImmediately(t *testing.T) {
	for _, message := range []string{"permission denied", "repository archived", "blob exceeds size limit", "policy must be a regular Git blob", "SSH host key refused", "invalid Git remote configuration"} {
		t.Run(message, func(t *testing.T) {
			_, err := ResolveRequirement(t.Context(), policyReader{err: errors.New(message)}, "upstream/org/repo", strings.Repeat("a", 40), nil)
			var unavailable *PolicyRefreshUnavailable
			if err == nil || errors.As(err, &unavailable) || !strings.Contains(err.Error(), message) {
				t.Fatalf("permanent policy failure was hidden as an outage: %v", err)
			}
		})
	}
}
