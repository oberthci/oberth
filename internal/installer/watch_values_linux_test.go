package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWatchValuesAreImmutableAcrossPreviewAndUpgrade(t *testing.T) {
	source := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(source, []byte("watchTunnel:\n  enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{WatchAdoptionPlan: "plan", ValuesFiles: []string{source}}
	frozen, closeValues, err := freezeWatchValues(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeValues()
	if err = os.WriteFile(source, []byte("watchTunnel:\n  enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(frozen.ValuesFiles[0])
	if err != nil || !strings.Contains(string(raw), `"enabled":true`) {
		t.Fatalf("frozen input changed: %v", err)
	}
	if err = os.WriteFile(frozen.ValuesFiles[0], []byte(`{}`), 0600); err == nil {
		t.Fatal("sealed values remained writable")
	}
}
