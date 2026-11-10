package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An admin command never creates a database: a mistyped --database or an
// unmounted volume is refused and leaves no file behind.
func TestOpenAdminClientRefusesMissingDatabase(t *testing.T) {
	directory := t.TempDir()
	empty := filepath.Join(directory, "empty.sqlite")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, want string
		absent           bool
	}{
		{name: "missing file", path: filepath.Join(directory, "oberht.sqlite"), want: "does not exist", absent: true},
		{name: "missing directory", path: filepath.Join(directory, "unmounted", "oberth.sqlite"), want: "does not exist", absent: true},
		{name: "missing with DSN query", path: filepath.Join(directory, "query.sqlite") + "?_pragma=foreign_keys(1)", want: "does not exist", absent: true},
		{name: "empty file", path: empty, want: "is empty"},
		{name: "directory", path: directory, want: "not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := OpenAdminClient(context.Background(), tc.path, Options{})
			if !errors.Is(err, ErrDatabaseMissing) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "--database") {
				t.Fatalf("OpenAdminClient(%s) = %v, want ErrDatabaseMissing mentioning %q and --database", tc.path, err, tc.want)
			}
			base, _, _ := strings.Cut(tc.path, "?")
			if _, statErr := os.Stat(base); tc.absent && !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("refused open left a file behind: %v", statErr)
			}
		})
	}
	if info, err := os.Stat(empty); err != nil || info.Size() != 0 {
		t.Fatalf("refused open initialized the empty file: %v %v", info, err)
	}
	existing := filepath.Join(directory, "oberth.sqlite")
	created, err := CreateGenesis(context.Background(), existing, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenAdminClient(context.Background(), existing, Options{})
	if err != nil {
		t.Fatalf("existing database refused: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}
