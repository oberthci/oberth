package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemedyFlagRemovalControl(t *testing.T) {
	source, err := filepath.Abs("installflags.go")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	declaration := `f.Bool("dry-run", false, "Review without applying.")`
	if strings.Count(string(data), declaration) != 1 {
		t.Fatal("control must rename exactly one real parser declaration")
	}
	directory := t.TempDir()
	replacement := filepath.Join(directory, "installflags.go")
	changed := strings.Replace(string(data), declaration, strings.Replace(declaration, "dry-run", "dry-ran", 1), 1)
	if err := os.WriteFile(replacement, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{source: replacement}})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(directory, "overlay.json")
	if err := os.WriteFile(overlayPath, overlay, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "go", "test", "-race", "-count=1", "-p=2", "-buildvcs=false", "-timeout=20m", "-overlay", overlayPath, "-run", "^TestRemediesThatWork$", "./cmd/oberth")
	command.Dir = "../.."
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "unknown flag --dry-run") || !strings.Contains(string(output), "docs/") {
		t.Fatalf("renaming a real flag must fail the contract on a documented command: %v\n%s", err, output)
	}
	t.Log("renaming install/upgrade --dry-run through a Go overlay failed the documented-command contract")
}
