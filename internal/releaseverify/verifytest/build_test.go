package verifytest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFixtureOverlayPreservesSourcePinAndMutation(t *testing.T) {
	directory, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "oberth-fixture-overlay-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	canonical, err := filepath.Abs("../release-cosign.pub")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(directory, "existing.json")
	raw, _ := json.Marshal(map[string]any{"Replace": map[string]string{"/fixture/source.go": "/fixture/mutated.go"}})
	if err = os.WriteFile(existing, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFLAGS", "-overlay="+existing)
	key, err := fixturePublicKey()
	if err != nil {
		t.Fatal(err)
	}
	files, err := fixtureOverlay(t.Context(), directory, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := overlayFiles(directory, files.overlay)
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := os.ReadFile(overlay[canonical])
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	want := struct{ source, embedded, mutation string }{string(before), string(key), "/fixture/mutated.go"}
	got := struct{ source, embedded, mutation string }{string(after), string(embedded), overlay["/fixture/source.go"]}
	if got != want {
		t.Fatal("fixture overlay changed source pin or discarded the mutation")
	}
}
