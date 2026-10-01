package auditanchor

import (
	"strings"
	"testing"
)

// The fixtures mirror rekor v1.5.3 pkg/sharding/sharding_test.go so the
// ported validators in rekoruuid.go stay pinned to upstream behavior. If a
// future rekor upgrade changes these semantics, this suite is the tripwire.
const (
	rekorTestValidTreeID1 = "0FFFFFFFFFFFFFFF"
	rekorTestValidTreeID2 = "3315648d077a9f02"
	rekorTestValidTreeID3 = "7241b7903737211c"
	rekorTestZeroTreeID   = "0000000000000000"
	rekorTestValidUUID    = "f794467401d57241b7903737211c721cb3315648d077a9f02ceefb6e404a05de"
	rekorTestNotHexTreeID = "ZZZZZZZZZZZZZZZZ"
	rekorTestNotHexUUID1  = "94467401d57241b7903737211c721cb3315648d077a9f02ceefb6e404a05dezq"
	rekorTestNotHexUUID2  = "y794467401d57241b7903737211c721cb3315648d077a9f02ceefb6e404a05de"
)

func TestRekorUUIDFromIDStringAcceptsValidForms(t *testing.T) {
	accepted := []string{
		rekorTestValidUUID,
		rekorTestValidTreeID1 + rekorTestValidUUID,
		rekorTestValidTreeID2 + rekorTestValidUUID,
		rekorTestValidTreeID3 + rekorTestValidUUID,
	}
	for _, id := range accepted {
		result, err := rekorUUIDFromIDString(id)
		if err != nil {
			t.Fatalf("rekorUUIDFromIDString(%q) returned error: %v", id, err)
		}
		if result != rekorTestValidUUID {
			t.Fatalf("rekorUUIDFromIDString(%q) = %q, want %q", id, result, rekorTestValidUUID)
		}
	}
}

func TestRekorUUIDFromIDStringToleratesZeroTreeID(t *testing.T) {
	// Upstream deliberately accepts an EntryID with TreeID zero and returns
	// its UUID; only the UUID part must still be well-formed hex.
	result, err := rekorUUIDFromIDString(rekorTestZeroTreeID + rekorTestValidUUID)
	if err != nil {
		t.Fatalf("zero-TreeID EntryID rejected: %v", err)
	}
	if result != rekorTestValidUUID {
		t.Fatalf("zero-TreeID EntryID returned %q, want %q", result, rekorTestValidUUID)
	}
	if _, err := rekorUUIDFromIDString(rekorTestZeroTreeID + rekorTestNotHexUUID2); err == nil {
		t.Fatal("zero-TreeID tolerance must not rescue a malformed UUID")
	}
}

func TestRekorUUIDFromIDStringRejectsMalformedInput(t *testing.T) {
	rejected := map[string]string{
		"too-long EntryID":       rekorTestValidTreeID1 + rekorTestValidUUID + "e",
		"too-short EntryID":      (rekorTestValidTreeID1 + rekorTestValidUUID)[:rekorEntryIDHexStringLen-1],
		"too-long UUID":          rekorTestValidUUID + "e",
		"too-short UUID":         rekorTestValidUUID[:rekorUUIDHexStringLen-1],
		"bare TreeID":            rekorTestValidTreeID3,
		"empty string":           "",
		"non-hex UUID suffix":    rekorTestNotHexUUID1,
		"non-hex UUID prefix":    rekorTestNotHexUUID2,
		"non-hex TreeID":         rekorTestNotHexTreeID + rekorTestValidUUID,
		"non-hex TreeID + UUID":  rekorTestNotHexTreeID + rekorTestNotHexUUID2,
		"TreeID overflows int64": strings.Repeat("f", rekorTreeIDHexStringLen) + rekorTestValidUUID,
	}
	for name, id := range rejected {
		if result, err := rekorUUIDFromIDString(id); err == nil {
			t.Errorf("%s: rekorUUIDFromIDString(%q) = %q, want error", name, id, result)
		}
	}
}

func TestRekorValidateTreeIDMatchesUpstreamSemantics(t *testing.T) {
	for _, valid := range []string{rekorTestValidTreeID1, rekorTestValidTreeID2, rekorTestValidTreeID3} {
		if err := rekorValidateTreeID(valid); err != nil {
			t.Fatalf("rekorValidateTreeID(%q) returned error: %v", valid, err)
		}
	}
	if err := rekorValidateTreeID(rekorTestZeroTreeID); err == nil {
		t.Fatal("zero TreeID must be reported by rekorValidateTreeID")
	}
	if err := rekorValidateTreeID("12345"); err == nil {
		t.Fatal("short TreeID must be rejected by rekorValidateTreeID")
	}
}
