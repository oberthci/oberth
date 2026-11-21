package installer_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
)

type snapshotFunc func(context.Context, stateport.Options, io.Writer) (stateport.BackupReport, error)

func (f snapshotFunc) Snapshot(ctx context.Context, o stateport.Options, w io.Writer) (stateport.BackupReport, error) {
	return f(ctx, o, w)
}

func backupConfig(t *testing.T) (installer.BackupConfig, *age.X25519Identity) {
	t.Helper()
	source, options := backupSource(t)
	key, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return installer.BackupConfig{Source: source, Options: options, Recipient: key.Recipient().String(), Path: filepath.Join(t.TempDir(), "backup.age")}, key
}

func TestBackupFailureRemovesOnlyItsIncompleteFile(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"source", "invalid report", "cancelled"} {
		t.Run(kind, func(t *testing.T) { failedBackupCase(t, kind) })
	}
}

func failedBackupCase(t *testing.T, kind string) {
	t.Helper()
	cfg, _ := backupConfig(t)
	orphan := filepath.Join(filepath.Dir(cfg.Path), ".oberth-backup-incomplete-unrelated")
	if err := os.WriteFile(orphan, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Source = snapshotFunc(func(ctx context.Context, _ stateport.Options, out io.Writer) (stateport.BackupReport, error) {
		assertBackupUnpublished(t, cfg.Path)
		if _, err := out.Write([]byte("partial snapshot")); err != nil {
			return stateport.BackupReport{}, err
		}
		if kind == "source" {
			return stateport.BackupReport{}, errors.New("snapshot failed")
		}
		if kind == "cancelled" {
			<-ctx.Done()
			return stateport.BackupReport{}, ctx.Err()
		}
		return stateport.BackupReport{}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if kind == "cancelled" {
		cancel()
	}
	if _, err := installer.Backup(ctx, cfg); err == nil {
		t.Fatal("failed snapshot accepted")
	}
	assertBackupUnpublished(t, cfg.Path)
	entries, err := os.ReadDir(filepath.Dir(cfg.Path))
	if err != nil || len(entries) != 1 {
		t.Fatal("failure retained its staging file or deleted another file", err)
	}
	assertFileContent(t, orphan, "keep")
}

func TestBackupRefusesExistingDestinationBeforeSnapshot(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"file", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _ := backupConfig(t)
			occupyBackupPath(t, cfg.Path, kind)
			cfg.Source = snapshotFunc(func(context.Context, stateport.Options, io.Writer) (stateport.BackupReport, error) {
				t.Fatal("existing destination reached snapshot")
				return stateport.BackupReport{}, nil
			})
			if _, err := installer.Backup(t.Context(), cfg); err == nil {
				t.Fatal("existing destination accepted")
			}
			assertOccupiedBackupPath(t, cfg.Path, kind)
		})
	}
}

func TestBackupPublicationCannotOverwriteConcurrentDestination(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"file", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			cfg, _ := backupConfig(t)
			source := cfg.Source
			cfg.Source = snapshotFunc(func(ctx context.Context, o stateport.Options, out io.Writer) (stateport.BackupReport, error) {
				assertBackupUnpublished(t, cfg.Path)
				occupyBackupPath(t, cfg.Path, kind)
				return source.Snapshot(ctx, o, out)
			})
			if _, err := installer.Backup(t.Context(), cfg); err == nil {
				t.Fatal("publication overwrote concurrent destination")
			}
			assertOccupiedBackupPath(t, cfg.Path, kind)
			entries, err := os.ReadDir(filepath.Dir(cfg.Path))
			if err != nil || len(entries) != 1 {
				t.Fatal("refused publication retained staging", err)
			}
		})
	}
}

func TestBackupPublishesCompletePrivateArchive(t *testing.T) {
	t.Parallel()
	cfg, key := backupConfig(t)
	source := cfg.Source
	cfg.Source = snapshotFunc(func(ctx context.Context, o stateport.Options, out io.Writer) (stateport.BackupReport, error) {
		assertBackupUnpublished(t, cfg.Path)
		entries, err := os.ReadDir(filepath.Dir(cfg.Path))
		if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".oberth-backup-incomplete-") {
			t.Fatal("snapshot has no distinguishable incomplete file", err)
		}
		info, err := entries[0].Info()
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("incomplete file is public", err)
		}
		return source.Snapshot(ctx, o, out)
	})
	archive, err := installer.Backup(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	assertCompleteBackupArchive(t, cfg, archive, key)
	assertPublishedBackupFile(t, cfg.Path)
}

func assertCompleteBackupArchive(t *testing.T, cfg installer.BackupConfig, archive installer.Archive, key *age.X25519Identity) {
	t.Helper()
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := age.Decrypt(bytes.NewReader(raw), key)
	if err != nil {
		t.Fatal(err)
	}
	inspector, _ := backupSource(t)
	report, err := inspector.Inspect(t.Context(), cfg.Options, decrypted)
	sum := sha256.Sum256(raw)
	if err != nil || report != archive.Report || archive.SHA256 != hex.EncodeToString(sum[:]) || archive.Path != cfg.Path {
		t.Fatal("published archive proof differs", err)
	}
}

func assertPublishedBackupFile(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("published archive is public", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatal("successful publication retained staging", err)
	}
}

func TestBackupKilledBeforeSnapshotPublication(t *testing.T) {
	t.Parallel()
	if root := os.Getenv("OBERTH_BACKUP_KILL_TEST"); root != "" {
		runKilledBackupChild(t, root)
		return
	}
	cfg, key := backupConfig(t)
	killBackupAtSnapshot(t, cfg)
	assertBackupUnpublished(t, cfg.Path)
	entries, err := os.ReadDir(filepath.Dir(cfg.Path))
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".oberth-backup-incomplete-") {
		t.Fatal("killed backup did not leave a distinguishable incomplete file", err)
	}
	orphan := filepath.Join(filepath.Dir(cfg.Path), entries[0].Name())
	info, err := os.Stat(orphan)
	if err != nil || info.Mode().Perm() != 0600 || info.Size() == 0 {
		t.Fatal("killed backup did not reach private encryption header", err)
	}
	raw, err := os.ReadFile(orphan)
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := age.Decrypt(bytes.NewReader(raw), key)
	if err != nil {
		t.Fatal("child did not write encryption header", err)
	}
	if _, err = io.ReadAll(decrypted); err == nil {
		t.Fatal("blocked snapshot unexpectedly complete")
	}
	if _, err = installer.Backup(t.Context(), cfg); err != nil {
		t.Fatal("killed backup blocks identical retry", err)
	}
	assertFileContent(t, orphan, string(raw))
}

func runKilledBackupChild(t *testing.T, root string) {
	t.Helper()
	_, options := backupSource(t)
	ready := os.NewFile(3, "snapshot-ready")
	defer func() { _ = ready.Close() }()
	source := snapshotFunc(func(ctx context.Context, _ stateport.Options, _ io.Writer) (stateport.BackupReport, error) {
		if _, err := ready.Write([]byte{1}); err != nil {
			return stateport.BackupReport{}, err
		}
		<-ctx.Done()
		return stateport.BackupReport{}, ctx.Err()
	})
	_, err := installer.Backup(t.Context(), installer.BackupConfig{Source: source, Options: options, Recipient: os.Getenv("OBERTH_BACKUP_KILL_RECIPIENT"), Path: filepath.Join(root, "backup.age")})
	t.Fatal("child backup returned before kill", err)
}

func killBackupAtSnapshot(t *testing.T, cfg installer.BackupConfig) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready, signal, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ready.Close(); _ = signal.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-test.run=^TestBackupKilledBeforeSnapshotPublication$", "-test.timeout=30s")
	command.Env = append(os.Environ(), "OBERTH_BACKUP_KILL_TEST="+filepath.Dir(cfg.Path), "OBERTH_BACKUP_KILL_RECIPIENT="+cfg.Recipient)
	command.ExtraFiles = []*os.File{signal}
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	if err = signal.Close(); err != nil {
		t.Fatal(err)
	}
	var marker [1]byte
	if _, err = io.ReadFull(ready, marker[:]); err != nil {
		t.Fatal("child did not reach snapshot", err)
	}
	if err = command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	waited = true
	if err == nil {
		t.Fatal("child was not killed", output.String())
	}
}

func assertBackupUnpublished(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("incomplete backup has final name", err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != want {
		t.Fatal("unrelated file changed", err)
	}
}

func occupyBackupPath(t *testing.T, path, kind string) {
	t.Helper()
	var err error
	switch kind {
	case "file":
		err = os.WriteFile(path, []byte("existing backup"), 0600)
	case "directory":
		err = os.Mkdir(path, 0700)
	case "symlink":
		err = os.Symlink("missing-target", path)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func assertOccupiedBackupPath(t *testing.T, path, kind string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "file":
		assertFileContent(t, path, "existing backup")
	case "directory":
		if !info.IsDir() {
			t.Fatal("existing directory replaced")
		}
	case "symlink":
		if target, err := os.Readlink(path); err != nil || target != "missing-target" {
			t.Fatal("existing symlink replaced", err)
		}
	}
}

func TestBackupPreservesUnrelatedRestoreFiles(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"invalid recipient", "existing destination", "success", "dry run"} {
		t.Run(kind, func(t *testing.T) { backupPreservesRestoreFileCase(t, kind) })
	}
}

func backupPreservesRestoreFileCase(t *testing.T, kind string) {
	t.Helper()
	cfg, _ := backupConfig(t)
	dir := filepath.Dir(cfg.Path)
	leftover := filepath.Join(dir, ".restore-personal-notes")
	content := []byte("unrelated file")
	if err := os.WriteFile(leftover, content, 0644); err != nil {
		t.Fatal(err)
	}
	cfg.DryRun = kind == "dry run"
	if kind == "invalid recipient" {
		cfg.Recipient = "invalid"
	}
	if kind == "existing destination" {
		if err := os.WriteFile(cfg.Path, []byte("existing backup"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := installer.Backup(t.Context(), cfg)
	if (err != nil) != (kind == "invalid recipient" || kind == "existing destination") {
		t.Fatal("unexpected backup result", err)
	}
	after, err := os.ReadFile(leftover)
	if err != nil || string(after) != string(content) {
		t.Fatal("backup changed unrelated .restore- file", err)
	}
	info, err := os.Stat(leftover)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("backup changed unrelated file mode", err)
	}
}

func backupSource(t *testing.T) (*statefake.Backup, stateport.Options) {
	t.Helper()
	options := stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: 1, MaxBytes: 1 << 20}
	report := stateport.BackupReport{Schema: 1, AuditHead: stateport.AuditHead{ID: 7, SHA256: strings.Repeat("a", 64)}, Integrity: true, ForeignKeys: true}
	source, err := statefake.New(options.Protocol, report, []byte("canonical verified seed"))
	if err != nil {
		t.Fatal(err)
	}
	return source, options
}
