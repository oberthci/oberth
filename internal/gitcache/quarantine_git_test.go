package gitcache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepGitQuarantineNames(t *testing.T) {
	repo := t.TempDir()
	objects := filepath.Join(repo, "objects")
	if err := os.Mkdir(objects, 0700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-receiveQuarantineMaxAge - time.Minute)
	for _, name := range []string{"tmp_objdir-incoming-old", "tmp_objdir-incoming-recent", "unrelated"} {
		p := filepath.Join(objects, name)
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
		if name != "tmp_objdir-incoming-recent" {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := sweepReceiveQuarantines(repo, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tmp_objdir-incoming-old", "tmp_objdir-incoming-recent", "unrelated"} {
		_, err := os.Stat(filepath.Join(objects, name))
		if os.IsNotExist(err) != (name == "tmp_objdir-incoming-old") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
