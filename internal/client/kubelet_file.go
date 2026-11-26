package client

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
	version, err := kubeletVersion(dir)
	if err != nil {
		return nil, true, fileFailure(path, "read", err)
	}
	data, err = readKubeletVersion(filepath.Join(dir, version), name)
	if err != nil {
		return nil, true, fileFailure(path, "read", err)
	}
	return data, true, nil
}

// kubeletVersion returns the one directory ..data names inside the mount.
func kubeletVersion(dir string) (string, error) {
	version, err := os.Readlink(filepath.Join(dir, "..data"))
	if err != nil || version == ".." || !strings.HasPrefix(version, "..") || strings.Contains(version, "/") {
		return "", errors.Join(err, errKubeletLayout)
	}
	if mount, err := os.Lstat(dir); err != nil || !mount.IsDir() || !kubeletOwned(mount, 0) {
		return "", errors.Join(err, errKubeletLayout)
	}
	if target, err := os.Lstat(filepath.Join(dir, version)); err != nil || !target.IsDir() || !kubeletOwned(target, 022) {
		return "", errors.Join(err, errKubeletLayout)
	}
	return version, nil
}

var errKubeletLayout = errors.New("kubelet volume layout is not safe")

func readKubeletVersion(dir, name string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := openClientFile(root, name)
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

func kubeletOwned(info fs.FileInfo, mask fs.FileMode) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == 0 || stat.Uid == uint32(os.Getuid())) && info.Mode().Perm()&mask == 0
}
