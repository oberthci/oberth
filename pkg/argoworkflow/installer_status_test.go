package argoworkflow

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func checkInstallerControllerStatus(t *testing.T, directory string) {
	t.Helper()
	actual, err := os.ReadFile(filepath.Join(directory, "installer-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("../../internal/installer/testdata/workflow-controller-status-payload.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("installer status sample differs from pinned controller write", string(actual))
	}
}
