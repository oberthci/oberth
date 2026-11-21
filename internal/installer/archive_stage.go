package installer

import (
	"errors"
	"os"
	"path/filepath"
)

// sameBackupFile catches accidents, not racing writers in a trusted directory.
func sameBackupFile(identity os.FileInfo, name string) error {
	named, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if identity == nil || !os.SameFile(identity, named) {
		return errors.New("the incomplete backup no longer names the recorded file")
	}
	return nil
}

func removeBackupStage(identity os.FileInfo, stage *string) error {
	name := *stage
	*stage = ""
	if name == "" {
		return nil
	}
	if err := sameBackupFile(identity, name); err != nil {
		return err
	}
	return os.Remove(name)
}

func syncParent(path string) error {
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

func closeBackupBeforePublish(file *os.File, prior error, path string, publish func() error) error {
	if err := errors.Join(prior, backupFailure("backup-staging", file.Close(), path)); err != nil {
		return err
	}
	return publish()
}
