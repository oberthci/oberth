package controllertest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFixtureRetainsCompilerFailureForEveryCaller(t *testing.T) {
	t.Parallel()
	module := t.TempDir()
	controller := filepath.Join(module, "workflow", "controller")
	if err := os.MkdirAll(controller, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module fixture\n\ngo 1.26.0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(controller, "input.go")
	if err := os.WriteFile(source, []byte("package controller\nvar _ = compilerOutputMustSurvive\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixture := New(nil)
	t.Cleanup(func() {
		if err := fixture.Close(); err != nil {
			t.Error(err)
		}
	})
	_, first := fixture.compilation(t.Context(), module)
	if first == nil || !strings.Contains(first.Error(), "undefined: compilerOutputMustSurvive") {
		t.Fatalf("expected compiler diagnostic, got %v", first)
	}
	if err := os.WriteFile(source, []byte("package controller\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var callers sync.WaitGroup
	results := make([]error, 8)
	for index := range results {
		callers.Go(func() {
			_, results[index] = fixture.compilation(t.Context(), module)
		})
	}
	callers.Wait()
	for index, err := range results {
		if !errors.Is(err, first) || err.Error() != first.Error() {
			t.Errorf("caller %d lost the original compiler failure: %v", index, err)
		}
	}
}
