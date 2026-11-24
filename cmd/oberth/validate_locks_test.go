package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestValidateLockPlacementIsSuccessfulFinding(t *testing.T) {
	for _, lock := range []string{"mutexes: [{name: forge/org/repo/build}]", "semaphores: [{configMapKeyRef: {name: oberth-sync-limits, key: build}}]"} {
		for _, carrier := range []string{"workflow", "steps"} {
			for _, format := range []string{"text", "json"} {
				validateLockCase(t, lock, carrier, format)
			}
		}
	}
}

func validateLockCase(t *testing.T, lock, carrier, format string) {
	t.Helper()
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.directory = t.TempDir()
	body := "apiVersion: argoproj.io/v1alpha1\nkind: Workflow\nspec:\n  entrypoint: parent\n  templates:\n  - name: parent\n    steps: [[{name: child, template: leaf}]]\n  - name: leaf\n    container: {image: example.test/image:1, command: ['true']}\n"
	where, replacement := "spec:\n", "spec:\n  synchronization: {"+lock+"}\n"
	path := "spec.synchronization"
	if carrier == "steps" {
		where, replacement = "  - name: parent\n", "  - name: parent\n    synchronization: {"+lock+"}\n"
		path = `spec.templates["parent"].synchronization`
	}
	body = strings.Replace(body, where, replacement, 1)
	if err := os.WriteFile(filepath.Join(app.directory, "workflow.yaml"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"validate", "workflow.yaml"}
	if format == "json" {
		args = append(args, "--json")
	}
	if code := app.run(t.Context(), args); code != 0 {
		t.Fatal("lock placement refused", carrier, code, diagnostic.String())
	}
	assertLockOutput(t, out, path, format)
}

func assertLockOutput(t *testing.T, out *bytes.Buffer, path, format string) {
	t.Helper()
	if format == "json" {
		var result validationResult
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.OK || len(result.Findings) != 1 || !result.Findings[0].Advisory || result.Findings[0].Path != path {
			t.Fatal("advisory absent from JSON", out.String(), err)
		}
	} else if !strings.Contains(out.String(), path) || !strings.Contains(out.String(), "leaf templates") {
		t.Fatal("advisory absent from text", out.String())
	}
}
