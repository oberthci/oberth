package client

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// Kubelet publishes Secret and ConfigMap volumes as name -> ..data/name and
// ..data -> ..<version>, both inside the mount, and swaps ..data atomically.
// A public or mounted-secret read accepts that layout: the version directory and file
// are owned by root or this user and writable by neither group nor others,
// and the file is opened without following links. Any other link, including
// one that leaves the mount, takes the ordinary refusal. ok is false when the
// path is not in this layout.
func readKubeletFile(path string) (data []byte, ok bool, err error) {
	dir, name := filepath.Dir(path), filepath.Base(path)
	if link, err := os.Readlink(path); err != nil || link != "..data/"+name {
		return nil, false, nil
	}
	mount, err := openKubeletMount(dir)
	if err != nil {
		return nil, true, fileFailure(path, "read", err)
	}
	defer mount.Close()
	version, err := kubeletVersion(mount, name)
	if err != nil {
		return nil, true, fileFailure(path, "read", err)
	}
	data, err = readKubeletVersion(mount, version, name)
	if err != nil {
		return nil, true, fileFailure(path, "read", err)
	}
	return data, true, nil
}

// kubeletVersion returns the one directory ..data names inside the mount.
func kubeletVersion(mount *os.File, name string) (string, error) {
	link, err := kubeletLink(mount, name)
	if err != nil || link != "..data/"+name {
		return "", errors.Join(err, errKubeletLayout)
	}
	version, err := kubeletLink(mount, "..data")
	if err != nil || version == ".." || !strings.HasPrefix(version, "..") || strings.Contains(version, "/") {
		return "", errors.Join(err, errKubeletLayout)
	}
	if info, err := mount.Stat(); err != nil || !info.IsDir() || !kubeletOwned(info, 0) {
		return "", errors.Join(err, errKubeletLayout)
	}
	return version, nil
}

var errKubeletLayout = errors.New("kubelet volume layout is not safe")

func readKubeletVersion(mount *os.File, version, name string) ([]byte, error) {
	dir, err := openKubeletAt(mount, version, true)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	if info, err := dir.Stat(); err != nil || !info.IsDir() || !kubeletOwned(info, 022) {
		return nil, errors.Join(err, errKubeletLayout)
	}
	file, err := openKubeletAt(dir, name, false)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !kubeletOwned(info, 022) {
		return nil, errors.Join(err, errKubeletLayout)
	}
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.Join(err, errKubeletLayout)
	}
	return data, nil
}

// The kernel resolves the operator's path to the mount's parent once; the mount
// itself and everything below it are opened from pinned descriptors without
// following links.
func openKubeletMount(path string) (*os.File, error) {
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	return openKubeletAt(parent, filepath.Base(path), true)
}

func openKubeletAt(dir *os.File, name string, directory bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
	if directory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(dir.Fd()), name, flags, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}

func kubeletLink(dir *os.File, name string) (string, error) {
	var target [4096]byte
	n, err := unix.Readlinkat(int(dir.Fd()), name, target[:])
	if err != nil || n == len(target) {
		return "", errors.Join(err, errKubeletLayout)
	}
	return string(target[:n]), nil
}

func kubeletOwned(info fs.FileInfo, mask fs.FileMode) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == 0 || stat.Uid == uint32(os.Getuid())) && info.Mode().Perm()&mask == 0
}
