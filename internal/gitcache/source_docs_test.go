package gitcache_test

import (
	"os"
	"strings"
	"testing"
)

func TestSourceDeliveryDocumentsOmittedEntries(t *testing.T) {
	data, err := os.ReadFile("../../docs/pipelines.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Symbolic links and submodule entries are not delivered.") {
		t.Fatal("source delivery documentation omits its unsupported entries")
	}
}
