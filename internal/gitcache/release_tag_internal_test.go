package gitcache

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func TestAnyForgeRejectionOfATagCreationIsFinal(t *testing.T) {
	t.Parallel()
	ruleset := []byte("To forge\n!\trefs/tags/v1.2.3:refs/tags/v1.2.3\t[remote rejected] (push declined due to repository rule violations)\nDone")
	for _, tc := range []struct {
		ref    string
		output []byte
		want   bool
	}{
		{"refs/tags/v1.2.3", ruleset, true},
		{"refs/tags/v1.2.3", []byte("To forge\nDone"), false},
		{"refs/heads/main", ruleset, false},
	} {
		if got := refusedTagCreation(model.Publication{Ref: tc.ref}, tc.output); got != tc.want {
			t.Fatalf("%s: final=%v, want %v", tc.ref, got, tc.want)
		}
	}
}
