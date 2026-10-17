package oberth

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestDeploySystemdFilesShipped pins issue 056: deploy/systemd/README.md tells
// operators to install files from that directory, and a too-broad ignore rule
// once kept them out of the repository. CI checks out the tracked tree, so a
// file the README names but git ignores fails here.
func TestDeploySystemdFilesShipped(t *testing.T) {
	t.Parallel()
	readme, err := os.ReadFile(filepath.Join("deploy", "systemd", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	named := regexp.MustCompile(`deploy/systemd/([A-Za-z0-9._-]+)`).FindAllSubmatch(readme, -1)
	if len(named) == 0 {
		t.Fatal("README names no deploy/systemd files")
	}
	for _, m := range named {
		path := filepath.Join("deploy", "systemd", string(m[1]))
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("README installs %s, which is not in the repository: %v", path, err)
			continue
		}
		if filepath.Ext(path) == "" && info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is installed as a program but is not executable", path)
		}
	}
}
