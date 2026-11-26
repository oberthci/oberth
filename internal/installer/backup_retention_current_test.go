package installer

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"
)

func TestRetentionPreservesOlderBackupsWithoutCurrentManifest(t *testing.T) {
	for _, mode := range []string{"missing", "junk", "bad-signature", "signed-partial", "wrong-name", "wrong-id", "signed-full"} {
		t.Run(mode, func(t *testing.T) { checkCurrentManifestPrerequisite(t, mode) })
	}
}

func checkCurrentManifestPrerequisite(t *testing.T, mode string) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	old := "oberth-backup-20261101T000000.000000000Z.age"
	current := filepath.Join(dir, "oberth-backup-20261102T000000.000000000Z.age")
	c := BackupConfig{signing: private, Installation: true, Path: current, Keep: 1}
	writeRetentionProof(t, c, filepath.Join(dir, old), "signed-full")
	writeRetentionProof(t, c, current, "signed-full")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	names, err := c.retainedArchives(root)
	if err != nil || len(names) != 2 {
		t.Fatal("fixture must have two eligible completed backups", err)
	}
	// directoryBackup selects older names before publishing the new backup.
	names = []string{old}
	if err = os.Remove(current + ".manifest.json"); err != nil {
		t.Fatal(err)
	}
	writeRetentionProof(t, c, current, mode)
	err = c.pruneVerifiedBackups(root, names)
	valid := mode == "signed-full"
	if (err == nil) != valid {
		t.Fatal("retention ignored the just-completed archive's manifest prerequisite", mode, err)
	}
	assertCurrentManifestRetention(t, root, old, valid)
	if _, err = os.Stat(current); err != nil {
		t.Fatal("current archive was removed", err)
	}
}

func assertCurrentManifestRetention(t *testing.T, root *os.Root, old string, valid bool) {
	t.Helper()
	for _, name := range []string{old, old + ".manifest.json"} {
		_, statErr := root.Lstat(name)
		if valid && !os.IsNotExist(statErr) || !valid && statErr != nil {
			t.Fatal("retention pruned recovery data before verifying the current manifest", name, statErr)
		}
	}
}
