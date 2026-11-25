package installer_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestInstallationBackupValidatesBeforePublishAndRetention(t *testing.T) {
	f := setup(t)
	installed := apply(t, f, prepare(t, f))
	installSnapshotFixture(t, f)
	f.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: snapshotHTTP(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/v1/sys/storage/raft/snapshot" {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("private-malformed-snapshot"))}, nil
			}
			return installationSnapshotResponse(t, r)
		})})
		return client, func() {}, err
	}
	dir := t.TempDir()
	old := filepath.Join(dir, "oberth-backup-20260101T000000.000000000Z.age")
	if err := os.WriteFile(old, []byte("prior-good-archive"), 0600); err != nil {
		t.Fatal(err)
	}
	database, options := backupFixture(t)
	_, err := f.engine.BackupInstallation(t.Context(), installer.BackupConfig{Source: database, Options: options, Recipient: installed.Profile.BackupRecipient, Directory: dir, Keep: 1})
	if err == nil || strings.Contains(err.Error(), "private-malformed") {
		t.Fatal("invalid store snapshot counted as a good backup or leaked content", err)
	}
	if names := retentionNames(t, dir); len(names) != 1 || filepath.Base(old) != names[0] {
		t.Fatal("validation failure published output or pruned the last good backup", names)
	}
	if raw, err := os.ReadFile(old); err != nil || string(raw) != "prior-good-archive" {
		t.Fatal("validation failure changed the prior archive", err)
	}
}

// corruptingStage adds bytes to the staged ciphertext that were never hashed,
// as a faulty disk or a concurrent writer would.
type corruptingStage struct {
	installer.SnapshotPort
	dir string
}

func (s corruptingStage) Snapshot(ctx context.Context, options stateport.Options, out io.Writer) (stateport.BackupReport, error) {
	report, err := s.SnapshotPort.Snapshot(ctx, options, out)
	stages, globErr := filepath.Glob(filepath.Join(s.dir, ".oberth-backup-incomplete-*"))
	if err != nil || globErr != nil || len(stages) != 1 {
		return report, errors.Join(err, globErr, errors.New("staged archive not found"))
	}
	file, err := os.OpenFile(stages[0], os.O_WRONLY, 0)
	if err != nil {
		return report, err
	}
	// Past the final ciphertext, so the encrypter never overwrites it.
	_, err = file.WriteAt([]byte("X"), 1<<20)
	return report, errors.Join(err, file.Close())
}

func TestBackupRereadsWrittenArchiveBeforePublishing(t *testing.T) {
	t.Parallel()
	source, options := backupFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.age")
	_, err = installer.Backup(t.Context(), installer.BackupConfig{Source: corruptingStage{SnapshotPort: source, dir: dir},
		Options: options, Recipient: identity.Recipient().String(), Path: path})
	if err == nil {
		t.Fatal("backup published bytes that differ from the encrypted stream")
	}
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("changed archive was published", err)
	}
}
