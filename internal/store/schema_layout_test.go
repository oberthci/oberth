package store

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

// Covers: REQ-C7-021
func TestSchemaNumberPinsBaselineLayout(t *testing.T) {
	t.Parallel()
	// A changed baseline requires a new version entry, preserving each pinned layout.
	layouts := map[int]string{
		7: "347aab8b2b2c42de10fa742160a9eea703789ef04b43208279184bdbaf8537f8",
		6: "2b60a816b2be80901757d902b06fc7e6e9aa9b513328fd67c4e2347481acc8e4",
		5: "a181c1bbcb6822bfde71da3298e68aea1645823e1b35a36e2b7f7b4129d7615a",
	}
	want, ok := layouts[Schema]
	if !ok {
		t.Fatalf("schema %d has no pinned baseline digest", Schema)
	}
	got := fmt.Sprintf("%x", sha256.Sum256([]byte(baseline)))
	if got != want {
		t.Fatalf("baseline changed without a schema bump: schema %d digest %s, want %s", Schema, got, want)
	}
}
