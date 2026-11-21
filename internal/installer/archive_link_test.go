package installer

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestBackupLinkFailuresExplainTheKernelCause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, code string
		cause      error
	}{
		{"exists", "restore-exists", syscall.EEXIST},
		{"prohibited", "backup-hardlink", syscall.EPERM},
		{"unsupported", "backup-hardlink", syscall.ENOTSUP},
		{"operation unsupported", "backup-hardlink", syscall.EOPNOTSUPP},
		{"cross device", "backup-hardlink", syscall.EXDEV},
		{"not implemented", "backup-hardlink", syscall.ENOSYS},
		{"disk full", "backup-publish", syscall.ENOSPC},
		{"link limit", "backup-publish", syscall.EMLINK},
		{"permission", "backup-publish", syscall.EACCES},
		{"io", "backup-publish", syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause := &os.LinkError{Op: "link", Old: ".oberth-backup-incomplete-gone", New: "backup.age", Err: tc.cause}
			err := backupLinkFailure(cause, "backup.age")
			var p *Problem
			var native *os.LinkError
			if !errors.As(err, &p) || p.Code != tc.code || !errors.Is(err, tc.cause) || !errors.As(err, &native) {
				t.Fatal("wrong link classification or error chain", err)
			}
			if !strings.Contains(err.Error(), "backup.age") || !strings.Contains(err.Error(), tc.cause.Error()) || strings.Contains(err.Error(), ".oberth-backup-incomplete-") {
				t.Fatal("lost path/cause or exposed removed staging name", err)
			}
		})
	}
}
