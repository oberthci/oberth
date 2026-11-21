package secretstore_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
)

func TestMountedFileChecksTheTargetItReads(t *testing.T) {
	link, next := checkedProjection(t)
	rotated := false
	permission := errors.New("fixture permission refusal")
	data, err := secretstore.ReadMountedFileChecked(link, 64, "key", func(info os.FileInfo) error {
		if info.Mode().Perm()&0004 != 0 {
			return permission
		}
		if err := os.Rename(next, link); err != nil {
			return err
		}
		rotated = true
		return nil
	})
	if err != nil || !rotated || string(data) != "safe" {
		t.Fatal("checked a different projection from the bytes read", string(data), err)
	}
	data, err = secretstore.ReadMountedFileChecked(link, 64, "key", func(info os.FileInfo) error {
		if info.Mode().Perm()&0004 != 0 {
			return permission
		}
		return nil
	})
	if !errors.Is(err, permission) || len(data) != 0 {
		t.Fatal("replacement target bypassed its own mode check", string(data), err)
	}
}

func checkedProjection(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	for name, mode := range map[string]os.FileMode{"safe": 0440, "public": 0644} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	link, next := filepath.Join(root, "key"), filepath.Join(root, "next")
	if err := os.Symlink("safe", link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("public", next); err != nil {
		t.Fatal(err)
	}

	return link, next
}
