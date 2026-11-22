package gitport

import (
	"strings"
	"testing"
)

// An empty forge repository is seeded by the person, not with Oberth's deploy
// key, which is only in the cluster.
func TestEmptyForgeRemedyNamesThePersonsOwnForgeAccess(t *testing.T) {
	remedy := (&EmptyRepositoryError{}).Remedy()
	for _, want := range []string{"with your own forge access", "deploy key is not on this machine", "then retry"} {
		if !strings.Contains(remedy, want) {
			t.Fatal("remedy sends the person to the wrong credentials", remedy)
		}
	}
}
