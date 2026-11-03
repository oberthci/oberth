//go:build linux

package vmrunner

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func readBeaconArtifact(ctx context.Context, buildRoot string) ([]byte, error) {
	// buildRoot comes from the host's completed build record, never the test
	// contract. NO_SYMLINKS covers every component below that directory.
	root, err := unix.Open(buildRoot, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("vmrunner: build artifact root is unavailable")
	}
	defer func() { _ = unix.Close(root) }()
	fd, err := unix.Openat2(root, BeaconArtifact, &unix.OpenHow{Flags: unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NONBLOCK, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return nil, errors.New("vmrunner: fixed artifact must be a regular file beneath its build root")
	}
	file := os.NewFile(uintptr(fd), BeaconArtifact)
	defer func() { _ = file.Close() }()
	var before, after unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return nil, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG || before.Nlink != 1 || before.Size < 1 || before.Size > MaxBeaconArtifactBytes {
		return nil, errors.New("vmrunner: artifact is not a bounded private regular file")
	}
	body, err := io.ReadAll(io.LimitReader(artifactContextReader{ctx: ctx, reader: file}, MaxBeaconArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != before.Size {
		return nil, errors.New("vmrunner: artifact size changed during capture")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	// A second bounded pass and descriptor metadata checks reject observed
	// concurrent mutation. Execution still uses only the first captured bytes.
	second := sha256.New()
	count, err := io.Copy(second, io.LimitReader(artifactContextReader{ctx: ctx, reader: file}, MaxBeaconArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if err := unix.Fstat(fd, &after); err != nil {
		return nil, err
	}
	first := sha256.Sum256(body)
	if count != before.Size || before.Dev != after.Dev || before.Ino != after.Ino || before.Size != after.Size || before.Mtim != after.Mtim || before.Ctim != after.Ctim || before.Mode != after.Mode || before.Nlink != after.Nlink || string(first[:]) != string(second.Sum(nil)) {
		return nil, errors.New("vmrunner: artifact changed during capture")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return body, nil
}

type artifactContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader artifactContextReader) Read(body []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(body)
}
