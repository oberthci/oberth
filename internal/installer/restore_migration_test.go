package installer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/store"
)

// The migration the target server runs on its first start is rehearsed on a
// copy before the swap; a database that does not migrate stops the restore
// with the target untouched, and the archive's files are never changed.
func TestRestoreRehearsesTheMigrationOnACopy(t *testing.T) {
	t.Parallel()
	latest := store.LatestMigrationVersion()
	const previous = "/data/pre-restore-x/ on PVC data"

	archive := queuedRestoreArchive(t)
	archive.Manifest.Database.Schema = latest - 1
	before := archive.Database.MainSHA256
	var out bytes.Buffer
	if err := preflightRestoreMigration(t.Context(), archive, latest, previous, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rehearsed on a copy") {
		t.Fatalf("output = %q", out.String())
	}
	if sum, _ := fileSHA256(archive.Database.Main); sum != before {
		t.Fatal("the rehearsal changed the restored database")
	}
	if entries, _ := os.ReadDir(filepath.Dir(archive.Database.Main)); len(entries) != 1 {
		t.Fatalf("scratch copy left behind: %v", entries)
	}

	// Same schema: nothing to rehearse. Another CLI schema: say so.
	out.Reset()
	archive.Manifest.Database.Schema = latest
	if err := preflightRestoreMigration(t.Context(), archive, latest, previous, &out); err != nil || out.Len() != 0 {
		t.Fatalf("same schema = %v, %q", err, out.String())
	}
	archive.Manifest.Database.Schema = latest - 1
	if err := preflightRestoreMigration(t.Context(), archive, latest+1, previous, &out); err != nil || !strings.Contains(out.String(), "cannot rehearse") || !strings.Contains(out.String(), previous) {
		t.Fatalf("other CLI schema = %v, %q", err, out.String())
	}

	// A database the server could not open refuses before the swap.
	broken := filepath.Join(t.TempDir(), "oberth.sqlite")
	if err := os.WriteFile(broken, bytes.Repeat([]byte("not a database "), 512), 0o600); err != nil {
		t.Fatal(err)
	}
	archive.Database = restoredDatabase{Main: broken}
	err := preflightRestoreMigration(t.Context(), archive, latest, previous, &out)
	if err == nil || !strings.Contains(err.Error(), "does not migrate") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("broken database = %v", err)
	}
}
