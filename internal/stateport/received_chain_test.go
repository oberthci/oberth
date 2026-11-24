package stateport_test

import (
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestReceivedChainDeduplicatesAndDoesNotAlias(t *testing.T) {
	prior := []string{"older", "old", "older", "oldest"}
	got := stateport.InheritReceivedHeads("old", "old", prior)
	if !slices.Equal(got, []string{"old", "older", "oldest"}) {
		t.Fatal(got)
	}
	got[1] = "caller edit"
	if prior[0] != "older" {
		t.Fatal("inherited evidence aliases its predecessor")
	}
	if got := stateport.InheritReceivedHeads("different", "old", prior); !slices.Equal(got, []string{"different"}) {
		t.Fatal("a broken chain inherited evidence", got)
	}
}
