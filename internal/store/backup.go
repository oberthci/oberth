package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/oberthci/oberth/internal/stateport"
)

// Backup implements the archive port for one database path. C9 owns encryption.
type Backup struct {
	// Progress observes completed snapshot page batches; configure it before use.
	// The offline helper uses it to reset its idle watchdog while copying pages.
	Progress   func()
	path       string
	snapshot   *snapshotConfig
	syncFile   func(string) error
	syncParent func(string) error
}

// NewBackup binds an existing database path without opening or migrating it.
func NewBackup(path string) (*Backup, error) {
	if path == "" || strings.ContainsAny(path, "?\x00#") || strings.HasPrefix(path, "file:") {
		return nil, refuse(invalid, nil)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, refuse(invalid, err)
	}
	return &Backup{path: absolute}, nil
}

func (s *Store) Snapshot(ctx context.Context, options stateport.Options, out io.Writer) (stateport.BackupReport, error) {
	return (&Backup{path: s.path}).Snapshot(ctx, options, out)
}

func (b *Backup) Snapshot(ctx context.Context, options stateport.Options, out io.Writer) (report stateport.BackupReport, resultErr error) {
	if err := backupInput(ctx, options, out != nil); err != nil {
		return report, err
	}
	lock, err := lockParent(filepath.Dir(b.path))
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, lock.Close()) }()
	stage, cleanup, err := newStage(filepath.Dir(b.path))
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()
	path := filepath.Join(stage, "oberth.sqlite")
	limit, _ := options.Limit()
	config := defaultSnapshot(limit)
	if b.snapshot != nil {
		config = *b.snapshot
		config.limit = limit
	}
	config.progress = b.Progress
	if err := snapshotWith(ctx, b.path, path, config); err != nil {
		return report, err
	}
	report, err = verifyDatabase(ctx, path, options)
	if err != nil {
		return report, err
	}
	err = emitArchive(ctx, archiveOutput{path, options, report, out})
	return report, err
}

func (b *Backup) Inspect(ctx context.Context, options stateport.Options, in io.Reader) (report stateport.BackupReport, resultErr error) {
	if err := backupInput(ctx, options, in != nil); err != nil {
		return report, err
	}
	file, cleanup, err := anonymousInspection(filepath.Dir(b.path), options)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()
	return inspectArchiveTo(ctx, options, in, archiveInspection{database: file, verify: func(ctx context.Context, options stateport.Options) (stateport.BackupReport, error) {
		return verifyInspection(ctx, file, options)
	}})
}

func (b *Backup) Restore(ctx context.Context, request stateport.RestoreRequest, in io.Reader) (report stateport.BackupReport, resultErr error) {
	if err := backupInput(ctx, request.Options, in != nil && request.Destination != ""); err != nil {
		return report, err
	}
	destination, err := restorePath(request.Destination)
	if err != nil {
		return report, err
	}
	if _, err := os.Lstat(destination); err == nil {
		return report, refuse(exists, nil)
	} else if !errors.Is(err, os.ErrNotExist) {
		return report, databaseError(err)
	}
	stage, cleanup, err := newStage(filepath.Dir(destination))
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errors.Join(resultErr, cleanup()) }()
	report, err = inspectArchive(ctx, request.Options, in, stage)
	if err != nil {
		return report, err
	}
	path := filepath.Join(stage, "oberth.sqlite")
	return report, b.publishRestore(ctx, path, destination)
}

func backupInput(ctx context.Context, options stateport.Options, present bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !present {
		return refuse(invalid, nil)
	}
	return options.Validate()
}

func restorePath(path string) (string, error) {
	if strings.ContainsAny(path, "?\x00#") || strings.HasPrefix(path, "file:") {
		return "", refuse(invalid, nil)
	}
	absolute, err := filepath.Abs(path)
	return absolute, err
}

func syncFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (b *Backup) publishRestore(ctx context.Context, path, destination string) error {
	fileSync := syncFile
	if b.syncFile != nil {
		fileSync = b.syncFile
	}
	if err := fileSync(path); err != nil {
		return databaseError(err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Link(path, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return refuse(exists, err)
		}
		return databaseError(err)
	}
	parentSync := syncDirectory
	if b.syncParent != nil {
		parentSync = b.syncParent
	}
	return errors.Join(parentSync(filepath.Dir(destination)), ctx.Err())
}
