package installer

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/oberthci/oberth/internal/stateport"
)

type restoreFiles struct {
	database, snapshot *os.File
	info               installationBackupInfo
	secrets            []backupSecret
	options            stateport.Options
	report             stateport.BackupReport
}

func (f *restoreFiles) close() {
	for _, secret := range f.secrets {
		for _, value := range secret.Data {
			clear(value)
		}
	}
	for _, file := range []*os.File{f.database, f.snapshot} {
		if file != nil {
			_ = file.Close()
		}
	}
}

func (f *restoreFiles) member(name string, inspect func(io.Reader) error) func(io.Reader) error {
	return func(in io.Reader) error {
		if f == nil {
			return inspect(in)
		}
		file := f.database
		if name == "store.snapshot" {
			file = f.snapshot
		}
		return inspect(io.TeeReader(in, file))
	}
}

func stageRestore(ctx context.Context, c RestoreReviewConfig) (*restoreFiles, RestoreReview, error) {
	f, err := newRestoreFiles(c.Options)
	if err != nil {
		return nil, RestoreReview{}, restoreProblem()
	}
	review, err := readRestore(ctx, c, f)
	if err != nil {
		f.close()
		return nil, RestoreReview{}, err
	}
	f.report = review.Database
	return f, review, nil
}

// Unlink before writing any plaintext: the kernel releases the files even on SIGKILL.
func newRestoreFiles(options stateport.Options) (*restoreFiles, error) {
	root := os.TempDir()
	limit, _ := options.Limit()
	var stats syscall.Statfs_t
	if syscall.Statfs("/dev/shm", &stats) == nil && uint64(stats.Bsize)*stats.Bavail > uint64(2*limit+(128<<10)) {
		root = "/dev/shm"
	}
	dir, err := os.MkdirTemp(root, "oberth-restore-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(dir)
	f := &restoreFiles{options: options}
	for _, target := range []**os.File{&f.database, &f.snapshot} {
		file, err := os.CreateTemp(dir, "private-*")
		if err != nil {
			f.close()
			return nil, err
		}
		*target = file
		if err = os.Remove(file.Name()); err != nil {
			f.close()
			return nil, errors.Join(err, os.Remove(file.Name()))
		}
	}
	return f, nil
}
