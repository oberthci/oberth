package model

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestEventKindsMatchContractGolden(t *testing.T) {
	kinds := []EventKind{EventRunFinished, EventRunHeld, EventRunLockWait, EventPromotionFinished, EventReleasePublished, EventPublicationFailed, EventRunUpdated, EventIssueUpdated}
	raw, err := json.Marshal(kinds)
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("testdata/event-kinds.golden")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != strings.TrimSpace(string(golden)) {
		t.Fatal("event contract changed", string(raw))
	}
	for _, k := range kinds {
		if !k.Valid() {
			t.Fatal("contract kind refused", k)
		}
	}
	for _, k := range []EventKind{"admin.recorded", "branch.deleted", "issue.ci_observed", "unknown"} {
		if k.Valid() {
			t.Fatal("unapproved event kind", k)
		}
	}
	if !EventIssueUpdated.Global() || EventRunUpdated.Global() {
		t.Fatal("global event scope changed")
	}
}
