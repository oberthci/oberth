package admissioncontract

import (
	"testing"

	"github.com/oberthci/oberth/internal/identityname"
)

func TestPreparationEnumerationsAreClosedAndZeroIsInvalid(t *testing.T) {
	for _, mode := range []Mode{ModeSubmit, ModeInspect} {
		if !mode.Valid() {
			t.Fatal(mode)
		}
	}
	for _, mode := range []Mode{"", "other", "Submit", " inspect"} {
		if mode.Valid() {
			t.Fatalf("accepts %q", mode)
		}
	}
	for _, state := range []EvidenceState{StateVerified, StateLiveRequired, StateUnknown} {
		if !state.Valid() {
			t.Fatal(state)
		}
	}
	for _, state := range []EvidenceState{"", "other", "Verified", "verified "} {
		if state.Valid() {
			t.Fatalf("accepts %q", state)
		}
	}
}

func TestGrantEvidenceReturnsTheSharedKey(t *testing.T) {
	evidence := GrantEvidence{Repository: "forge/org/repo", File: ".oberth/release.yaml", Step: "publish", Kind: "secret", Path: "public/key", State: StateVerified}
	want := identityname.Key{Repository: evidence.Repository, File: evidence.File, Step: evidence.Step}
	if evidence.Key() != want {
		t.Fatal("evidence changes the identity/grant tuple")
	}
}
