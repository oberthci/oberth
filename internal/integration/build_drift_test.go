package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMakeTestScriptsExist(t *testing.T) {
	root := filepath.Join("..", "..")
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range strings.Fields(string(data)) {
		if !strings.HasPrefix(field, "./hack/") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, field)); err != nil {
			t.Errorf("Makefile references missing check %s: %v", field, err)
		}
	}
}
