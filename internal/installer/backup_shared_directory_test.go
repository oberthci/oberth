package installer_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestSharedBackupDirectoryRequiresOptInAndNeverPrunesOtherInstallation(t *testing.T) {
	source, options := backupFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	one := installer.FullArchiveForTest(installer.BackupConfig{Source: source, Options: options, Recipient: identity.Recipient().String(), Directory: dir, Keep: 1, Now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)})
	old, err := installer.Backup(t.Context(), one)
	if err != nil {
		t.Fatal(err)
	}
	two := installer.FullArchiveForTest(one)
	two.Now, two.AllowSharedDirectory = one.Now.AddDate(0, 0, 1), true
	foreign, err := installer.Backup(t.Context(), two)
	if err != nil {
		t.Fatal(err)
	}
	before := dirNames(t, dir)
	one.Now = two.Now.AddDate(0, 0, 1)
	_, err = installer.Backup(t.Context(), one)
	if err == nil || publicerr.Classify(err).Reason() != "Backups belong to another installation." || !slices.Equal(before, dirNames(t, dir)) {
		t.Fatal("shared directory was accepted or changed without opt-in", err)
	}
	one.AllowSharedDirectory = true
	current, err := installer.Backup(t.Context(), one)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{foreign.Path, foreign.Path + ".manifest.json", current.Path, current.Path + ".manifest.json"} {
		if _, err = os.Stat(path); err != nil {
			t.Fatal("own retention removed another installation's backup", filepath.Base(path), err)
		}
	}
	if _, err = os.Stat(old.Path); !os.IsNotExist(err) {
		t.Fatal("own retention did not prune the expired own archive", err)
	}
}
