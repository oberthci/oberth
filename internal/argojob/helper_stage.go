package argojob

import (
	"errors"
	"syscall"
)

// Reserve space for the archive and the verified tree together. The extra
// 512 MiB leaves headroom for filesystem blocks and directories.
const rerunStageBytes = maxRerunArchive + maxRerunTree + (512 << 20)
const rerunStageReason = "rerun stage has too little disk space"
const rerunStageRemedy = "Free at least 1536 MiB on the stage disk, or use a fresh-Pod resume or push for a full run."

var errRerunStageSpace = errors.New(rerunStageReason)

func rerunStageError(err error) error {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return errRerunStageSpace
	}
	return err
}

func (r holdRuntime) rerunStageRoot() string {
	if r.stageRoot != "" {
		return r.stageRoot
	}
	return holdStage
}

func checkRerunSpace(path string, need uint64) error {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return err
	}
	if fs.Bsize <= 0 || fs.Bavail <= 0 || uint64(fs.Bavail) < (need+uint64(fs.Bsize)-1)/uint64(fs.Bsize) {
		return errRerunStageSpace
	}
	return nil
}
