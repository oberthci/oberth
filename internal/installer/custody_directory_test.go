package installer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlyCustodyLeavesNoDirectories(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "new-config", "profile")
	c, err := OpenConfigDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("read-only custody created directories", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{path, filepath.Dir(path)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("left newly created directory behind", err)
		}
	}
}

func TestCustodyCloseKeepsAppliedAndExistingDirectories(t *testing.T) {
	for _, existing := range []bool{true, false} {
		path := filepath.Join(t.TempDir(), "profile")
		if existing {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		}
		c, err := OpenConfigDir(path)
		if err != nil {
			t.Fatal(err)
		}
		if !existing {
			if err := c.save("recovery", []byte("keep")); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatal("removed applied or existing profile", err)
		}
	}
}
