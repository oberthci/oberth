package installer

import (
	"context"
	"crypto/ed25519"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
)

const retainedBackupPrefix = "oberth-backup-"

// directoryBackup retains only full installation archives: a database-only
// archive cannot be restored, so it may neither count toward nor trigger pruning.
func (c BackupConfig) directoryBackup(ctx context.Context) (Archive, error) {
	if err := c.retentionInput(); err != nil {
		return Archive{}, err
	}
	root, err := os.OpenRoot(c.Directory)
	if err != nil {
		return Archive{}, retentionProblem()
	}
	defer root.Close()
	dir, err := root.Open(".")
	if err != nil {
		return Archive{}, retentionProblem()
	}
	defer dir.Close()
	if err = syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return Archive{}, retentionProblem()
	}
	names, err := c.retainedArchives(root)
	if err != nil {
		return Archive{}, err
	}
	c.Path = filepath.Join(c.Directory, retainedBackupPrefix+c.Now.UTC().Format(backupTimeFormat)+".age")
	c.Directory = ""
	archive, err := Backup(ctx, c)
	if err != nil || c.DryRun {
		return archive, err
	}
	proof, ok := retainedOrigin(root, filepath.Base(c.Path))
	if !ok || proof.Origin.InstallationID != c.installationID() {
		return archive, backupFailure("backup-retention", backupOriginProblem(), c.Path)
	}
	if err = pruneBackups(root, names, c.Keep); err != nil {
		return archive, backupFailure("backup-retention", err, c.Path)
	}
	return archive, backupDirectorySyncFailure(dir.Sync(), c.Path)
}

func (c BackupConfig) retentionInput() error {
	if !c.Installation && !c.DryRun {
		return DatabaseOnlyRetentionProblem()
	}
	if c.Path != "" || c.Keep < 1 || c.Keep > 1000 || c.Now.IsZero() {
		return retentionProblem()
	}
	return nil
}

func (c BackupConfig) retainedArchives(root *os.Root) ([]string, error) {
	if c.DryRun {
		return nil, nil
	}
	if len(c.signing) != ed25519.PrivateKeySize {
		return nil, backupSigningProblem()
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		stamp, ok := strings.CutPrefix(entry.Name(), retainedBackupPrefix)
		stamp, suffix := strings.CutSuffix(stamp, ".age")
		_, parse := time.Parse(backupTimeFormat, stamp)
		if !entry.Type().IsRegular() || !ok || !suffix || parse != nil {
			continue
		}
		proof, valid := retainedOrigin(root, entry.Name())
		if !valid {
			continue
		}
		if proof.Origin.InstallationID != c.installationID() {
			if !c.AllowSharedDirectory {
				return nil, sharedBackupDirectoryProblem()
			}
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

func pruneBackups(root *os.Root, names []string, keep int) error {
	slices.Sort(names)
	slices.Reverse(names)
	for _, name := range names[min(keep-1, len(names)):] {
		if err := root.Remove(name); err != nil {
			return err
		}
		if err := removeRetainedOrigin(root, name); err != nil {
			return err
		}
	}
	return nil
}

func removeRetainedOrigin(root *os.Root, archive string) error {
	name := archive + ".manifest.json"
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		return root.Remove(name)
	}
	return nil
}

func retentionProblem() error {
	return publicerr.New(publicerr.InvalidInput, "Backup retention could not finish.", "Use a writable backup directory and --keep from 1 to 1000; check the completed archive before retrying.", nil)
}

// DatabaseOnlyRetentionProblem refuses retention for archives restore cannot use.
func DatabaseOnlyRetentionProblem() error {
	return publicerr.New(publicerr.InvalidInput, "Retention needs a full backup.", "Drop --directory and --keep for a database-only copy; run a full backup to apply retention.", nil)
}
