package installer_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenewalRemovesAbandonedAdministratorStagingFiles(t *testing.T) {
	t.Parallel()
	f := setup(t)
	first := apply(t, f, prepare(t, f))
	orphan := filepath.Join(f.dir, "admin-"+strings.Repeat("e", 64)+".key.tmp")
	if err := os.WriteFile(orphan, []byte("abandoned-private-material"), 0600); err != nil {
		t.Fatal(err)
	}
	kept := []string{"admin-short.key.tmp", "admin-" + strings.Repeat("A", 64) + ".key.tmp", "other-" + strings.Repeat("e", 64) + ".key.tmp"}
	for _, name := range kept {
		if err := os.WriteFile(filepath.Join(f.dir, name), []byte("unrelated"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	linked := filepath.Join(f.dir, "admin-"+strings.Repeat("f", 64)+".key.tmp")
	if err := os.Symlink(first.Profile.AdminKey, linked); err != nil {
		t.Fatal(err)
	}
	second := apply(t, f, prepare(t, f))
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatal("renewal retained an abandoned administrator staging file", err)
	}
	kept = append(kept, filepath.Base(linked), filepath.Base(second.Profile.AdminKey), filepath.Base(second.Profile.AdminCert), filepath.Base(second.Profile.CAFile), filepath.Base(second.Profile.KnownHosts))
	for _, name := range kept {
		if _, err := os.Lstat(filepath.Join(f.dir, name)); err != nil {
			t.Fatal("renewal removed a live or unrelated file", name, err)
		}
	}
}
