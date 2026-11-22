package gitcache

import (
	"testing"
)

// A rejected tag push is final at the gate, whatever the forge's wording; a
// publication after the run keeps its retry for anything but a policy refusal.
func TestTagRejectionIsFinalOnlyAtTheGate(t *testing.T) {
	t.Parallel()
	rejected := func(summary string) []byte {
		return []byte("To forge\n!\trefs/tags/v1.2.3:refs/tags/v1.2.3\t" + summary + "\nDone")
	}
	ruleset := rejected("[remote rejected] (push declined due to repository rule violations)")
	for name, tc := range map[string]struct {
		output []byte
		gate   bool
		final  bool
	}{
		"gate, ruleset":               {ruleset, true, true},
		"gate, unpacker error":        {rejected("[remote rejected] (unpacker error)"), true, true},
		"gate, no rejection":          {[]byte("To forge\nDone"), true, false},
		"publication, ruleset":        {ruleset, false, false},
		"publication, unpacker error": {rejected("[remote rejected] (unpacker error)"), false, false},
		"publication, hook declined":  {rejected("[remote rejected] (pre-receive hook declined)"), false, true},
	} {
		if got := finalRejection(publicationAttempt{output: tc.output, gate: tc.gate}); got != tc.final {
			t.Fatalf("%s: final=%v, want %v", name, got, tc.final)
		}
	}
}
