package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExecutionPlacementAdministratorFile(t *testing.T) {
	if err := validateArgoServeOptions(serveOptions{argoExecutionPlacementConfig: "/etc/oberth/placement.json"}); err == nil {
		t.Fatal("placement ignored without Argo")
	}
	if p, err := readArgoExecutionPlacement(""); err != nil || p != nil {
		t.Fatal("default changed")
	}
	for _, name := range []string{"relative.json", "/etc/oberth/../placement.json", filepath.Join(t.TempDir(), "absent.json")} {
		if _, err := readArgoExecutionPlacement(name); err == nil {
			t.Fatal("invalid administrator path accepted")
		}
	}
	name := filepath.Join(t.TempDir(), "placement.json")
	for _, body := range []string{`{}`, `{"schema":"oberth.execution-placement/v1","schema":"other"}`, `{"unknown":"x"}`, string(make([]byte, 65537))} {
		if err := os.WriteFile(name, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readArgoExecutionPlacement(name); err == nil {
			t.Fatal("invalid authority file accepted")
		}
	}
}
