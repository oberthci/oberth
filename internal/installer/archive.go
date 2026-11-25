package installer

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/stateport"
)

// SnapshotPort is the authenticated service's canonical database stream.
// Its errors must be safe to display to the backup caller.
type SnapshotPort interface {
	Snapshot(context.Context, stateport.Options, io.Writer) (stateport.BackupReport, error)
}

// Archive describes either a completed encrypted backup or a dry-run destination.
// DryRun results carry no digest or database inspection report.
type Archive struct {
	DryRun            bool                   `json:"dry_run,omitempty"`
	StoreSnapshotHint string                 `json:"store_snapshot_hint,omitempty"`
	Path              string                 `json:"path"`
	SHA256            string                 `json:"sha256,omitempty"`
	Report            stateport.BackupReport `json:"report,omitzero"`
}
type BackupConfig struct {
	Source       SnapshotPort
	Options      stateport.Options
	Recipient    string
	Path         string
	DryRun       bool
	Installation bool
	Directory    string
	Keep         int
	Now          time.Time
	signing      ed25519.PrivateKey
	manifest     *bundleManifest
}

// Backup publishes a complete encrypted database without replacing a destination.
// Write to a local disk with hard-link support, then copy the completed backup.
// Retries leave prior incomplete files alone.
func Backup(ctx context.Context, c BackupConfig) (archive Archive, err error) {
	if c.Directory != "" {
		return c.directoryBackup(ctx)
	}
	if err = c.Options.Validate(); err != nil {
		return archive, err
	}
	if c.Source == nil {
		return archive, problem("wiring", nil)
	}
	recipient, err := age.ParseX25519Recipient(c.Recipient)
	if err != nil {
		return archive, problem("backup", err)
	}
	if err = absent(c.Path); err != nil {
		return archive, err
	}
	if err = backupParent(c.Path); err != nil {
		return archive, err
	}
	archive.Path = c.Path
	if c.DryRun {
		archive.DryRun = true
		return archive, ctx.Err()
	}
	return c.write(ctx, recipient)
}

func (c BackupConfig) write(ctx context.Context, recipient age.Recipient) (archive Archive, err error) {
	file, err := os.CreateTemp(filepath.Dir(c.Path), ".oberth-backup-incomplete-*")
	if err != nil {
		return archive, backupFailure("backup-staging", err, c.Path)
	}
	identity, statErr := file.Stat()
	stage := file.Name()
	defer func() {
		if file != nil {
			err = errors.Join(err, backupFailure("backup-staging", file.Close(), c.Path))
		}
		err = errors.Join(err, backupFailure("backup-staging", removeBackupStage(identity, &stage), c.Path))
	}()
	if statErr != nil {
		return archive, backupFailure("backup-staging", statErr, c.Path)
	}
	archive, err = c.writeSnapshot(ctx, file, recipient)
	if err == nil {
		err = verifyWrittenArchive(ctx, file, archive.SHA256)
	}
	if err == nil && c.Installation {
		err = c.publishOrigin(archive)
	}
	opened := file
	file = nil
	err = closeBackupBeforePublish(opened, err, c.Path, func() error {
		return c.publish(identity, &stage, &archive)
	})
	return archive, err
}

func (c BackupConfig) publish(identity os.FileInfo, stage *string, archive *Archive) error {
	if err := sameBackupFile(identity, *stage); err != nil {
		*stage = ""
		return backupFailure("backup-staging", err, c.Path)
	}
	if err := os.Link(*stage, c.Path); err != nil {
		return backupLinkFailure(err, c.Path)
	}
	archive.StoreSnapshotHint = StoreSnapshotHint
	if c.Installation {
		archive.StoreSnapshotHint = "Includes installation Secrets; bundled store data is included when configured."
	}
	if err := removeBackupStage(identity, stage); err != nil {
		return backupFailure("backup-cleanup", err, c.Path)
	}
	return backupDirectorySyncFailure(syncParent(c.Path), c.Path)
}

func (c BackupConfig) writeSnapshot(ctx context.Context, file *os.File, recipient age.Recipient) (archive Archive, err error) {
	archive.Path = c.Path
	limit, _ := c.Options.Limit()
	digest := sha256.New()
	encrypted, err := age.Encrypt(io.MultiWriter(file, digest), recipient)
	if err != nil {
		return archive, backupFailure("backup", err, c.Path)
	}
	archive.Report, err = c.Source.Snapshot(ctx, c.Options, &archiveWriter{ctx: ctx, out: encrypted, left: limit + (64 << 10)})
	err = errors.Join(err, encrypted.Close())
	if err != nil {
		return archive, backupFailure("backup", err, c.Path)
	}
	if err = backupReport(archive.Report, c.Options); err != nil {
		return archive, err
	}
	if err = file.Sync(); err != nil {
		return archive, backupFailure("backup-staging", err, c.Path)
	}
	archive.SHA256 = hex.EncodeToString(digest.Sum(nil))
	return archive, nil
}

func absent(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return described("restore-exists", nil, path)
	} else if !isAbsent(err) {
		return described("backup-destination", err, path)
	}
	return nil
}

type archiveWriter struct {
	ctx  context.Context
	out  io.Writer
	left int64
}

func (w *archiveWriter) Write(b []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(b)) > w.left {
		return 0, problem("backup-limit", nil)
	}
	n, err := w.out.Write(b)
	w.left -= int64(n)
	return n, err
}

func backupReport(r stateport.BackupReport, o stateport.Options) error {
	if r.Schema != o.Schema {
		return problem("restore-proof", nil)
	}
	return r.Validate()
}

func backupParent(path string) error {
	if path == "" {
		return described("backup-destination", os.ErrInvalid, "Choose a nonempty backup path.")
	}
	info, err := os.Stat(filepath.Dir(path))
	if err == nil && !info.IsDir() {
		err = os.ErrInvalid
	}
	if err != nil {
		return described("backup-destination", err, path)
	}
	return nil
}
