package main

import (
	"os"
	"strings"
	"testing"
)

func TestHoldProtocolUpgradeNotes(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../docs/plans/c9-install-and-upgrade.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, phrase := range []string{"Protocol 2 requires a matching server and helper", "before upgrade or rollback, release old holds", "mismatched holds are invisible and fail natively at expiry", "changed sealed specs refuse resume"} {
		if !strings.Contains(string(body), phrase) {
			t.Fatal("hold protocol upgrade note missing", phrase)
		}
	}
}
