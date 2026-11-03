package api

import "testing"

func TestKnownToolLookupDoesNotRebuildDefinitions(t *testing.T) {
	if !knownTool("status") || knownTool("does-not-exist") {
		t.Fatal("incorrect registry membership")
	}
	allocations := testing.AllocsPerRun(10, func() { _ = knownTool("issue_get") })
	if allocations > 1 {
		t.Fatalf("tool lookup allocates %.0f times per call", allocations)
	}
}
