package gitport

import (
	"strings"
	"testing"
)

func TestEmptyForgeRemedyNamesAdministratorPushThroughOberth(t *testing.T) {
	remedy := (&EmptyRepositoryError{}).Remedy()
	for _, want := range []string{"As administrator", "first commit", "default branch", "oberth remote"} {
		if !strings.Contains(remedy, want) {
			t.Fatal("remedy sends the person to the wrong credentials", remedy)
		}
	}
}
