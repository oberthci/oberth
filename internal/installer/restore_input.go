package installer

import (
	"context"
	"io"
	"os"
	"syscall"
)

func openBackupArchive(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, restoreProblem()
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, restoreProblem()
	}
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = file.Close()
		return nil, restoreProblem()
	}
	return file, nil
}

type archiveReader struct {
	ctx context.Context
	in  io.Reader
}

func (r archiveReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.in.Read(b)
}
