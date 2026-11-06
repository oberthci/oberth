package vmrunner

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Generic identities exercise the existing journal wire format. Original
// installed-recipe compatibility evidence remains in the external review packet;
// this fixture is not an installation identity, approval or network fetch.
func TestProtectedDescriptorPreservesJournalWire(t *testing.T) {
	body, err := os.ReadFile("testdata/protected-intent-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	body = bytes.TrimSpace(body)
	var intent KVMIntent
	if err = json.Unmarshal(body, &intent); err != nil {
		t.Fatal(err)
	}
	if err = intent.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, body) {
		t.Fatal("existing journal intent bytes changed")
	}
	if got := KVMIntentDigest(intent); got != "c41a672051491b8abefc496b10471afeb486a969d40bf80cd02b110f8938fcf7" {
		t.Fatalf("journal intent digest changed: %s", got)
	}
}

func TestProtectedRecipePreservesHarnessReferenceBoundary(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	for _, ref := range []string{
		"registry.example/harness" + digest, "registry.example:5000/repo/tag:v1" + digest,
		"", "harness:latest", digest, "@" + digest, "repo@@sha256:" + strings.Repeat("a", 64),
		"repo@sha256:" + strings.Repeat("A", 64), "repo@sha256:" + strings.Repeat("a", 63),
		"repo@sha512:" + strings.Repeat("a", 64), "repo name" + digest, "repo\x00name" + digest,
		"-repo" + digest, "repo?token=secret" + digest, "repo" + digest + "\n",
	} {
		t.Run(ref, func(t *testing.T) {
			recipe := KVMRecipe{SuiteSHA256: strings.Repeat("a", 64), GuestSHA256: strings.Repeat("b", 64), HarnessImage: ref, SuiteURL: "https://example.org/suite", GuestURL: "https://example.org/guest"}
			if got, want := recipe.Validate() == nil, len(validateGuestImageRef(ref)) == 0; got != want {
				t.Fatalf("extracted recipe changed harness boundary: got %v want %v", got, want)
			}
		})
	}
}
