package argojob_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/argoproj/argo-workflows/v4/workflow/common"
)

// The controller records actual Pod mounts. Constants here assert that the
// exercised cases include staging and identity; they do not supply the inventory.
func checkControllerMounts(t *testing.T, directory string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(directory, "*-mounts.json"))
	if err != nil || len(files) == 0 {
		t.Fatal("controller mount inventory missing", err)
	}
	var staging, identity bool
	for _, file := range files {
		var mounts []string
		readNativeJSON(t, file, &mounts)
		checkReservedProductMounts(t, mounts)
		staging = staging || slices.Contains(mounts, common.ExecutorStagingEmptyDir)
		identity = identity || slices.Contains(mounts, common.ServiceAccountTokenMountPath)
	}
	if !staging || !identity {
		t.Fatal("controller did not exercise staging and executor identity", staging, identity)
	}
}
