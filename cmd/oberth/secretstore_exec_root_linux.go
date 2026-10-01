//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Tests replace only the root chmod to exercise a prefetch refusal without a
// privileged read-only tmpfs mount.
var secretExecRootFchmod = unix.Fchmod

func defaultSecretExecFstatfs(fd int) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(fd, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Type), nil //nolint:unconvert // Linux architectures differ.
}

// prepareSecretExecRoot pins every path component without following symlinks.
// An existing emptyDir may have a public mode or a sticky tmpfs root. Only its
// owner may tighten it, and no credential is fetched until the pinned root is
// rechecked at private mode.
func prepareSecretExecRoot(directory string) (*os.File, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == "/" {
		return nil, errors.New("secret directory must be a clean absolute path below root")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open secret directory root: %w", err)
	}
	components := strings.Split(strings.TrimPrefix(directory, "/"), "/")
	for index, component := range components {
		next, openErr := unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(openErr, unix.ENOENT) && index == len(components)-1 {
			kind, kindErr := secretExecFstatfs(fd)
			if kindErr != nil || kind != tmpfsMagic {
				_ = unix.Close(fd)
				return nil, errors.New("secret directory parent is not verified tmpfs")
			}
			if makeErr := unix.Mkdirat(fd, component, secretExecDirMode); makeErr != nil && !errors.Is(makeErr, unix.EEXIST) {
				_ = unix.Close(fd)
				return nil, fmt.Errorf("create private secret directory: %w", makeErr)
			}
			next, openErr = unix.Openat(fd, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		_ = unix.Close(fd)
		if openErr != nil {
			return nil, fmt.Errorf("open secret directory without following links: %w", openErr)
		}
		fd = next
	}
	root := os.NewFile(uintptr(fd), directory)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("inspect secret directory: %w", err)
	}
	kind, err := secretExecFstatfs(fd)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("inspect secret directory filesystem: %w", err)
	}
	// These numbers are safe to include in a prefetch failure: they describe
	// only the server-owned mount root, never a credential or directory entry.
	euid := secretExecEUID()
	metadata := fmt.Sprintf("uid=%d gid=%d euid=%d mode=%#o fstype=%#x", stat.Uid, stat.Gid, euid, stat.Mode&07777, kind)
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = root.Close()
		return nil, fmt.Errorf("secret directory is not a directory (%s)", metadata)
	}
	if kind != tmpfsMagic {
		_ = root.Close()
		return nil, fmt.Errorf("secret directory is not verified tmpfs (%s)", metadata)
	}
	// Kubelet can leave an owned memory-backed emptyDir root sticky. Refuse
	// setuid/setgid and any other owner's root; the sticky bit itself is
	// removed by Fchmod on this no-symlink descriptor before Vault setup.
	if int64(stat.Uid) != int64(euid) || stat.Mode&06000 != 0 {
		_ = root.Close()
		return nil, fmt.Errorf("secret directory must be owned by the effective user without setuid/setgid (%s)", metadata)
	}
	if stat.Mode&07777 != secretExecDirMode {
		if err := secretExecRootFchmod(fd, secretExecDirMode); err != nil {
			_ = root.Close()
			return nil, fmt.Errorf("tighten secret directory permissions (%s): %w", metadata, err)
		}
	}
	if err := checkSecretExecRoot(root); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

func checkSecretExecRoot(root *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &stat); err != nil {
		return fmt.Errorf("inspect pinned secret directory: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || int64(stat.Uid) != int64(secretExecEUID()) ||
		stat.Mode&07777 != secretExecDirMode {
		return errors.New("secret directory is not owner-private")
	}
	kind, err := secretExecFstatfs(int(root.Fd()))
	if err != nil || kind != tmpfsMagic {
		return errors.New("secret directory is not verified tmpfs")
	}
	return nil
}

// writeSecretTreeFD confines every component to the verified root descriptor.
func writeSecretTreeFD(root *os.File, name string, values map[string][]byte) error {
	if err := checkSecretExecRoot(root); err != nil {
		return err
	}
	if err := validateSecretSegment("local name", name); err != nil {
		return err
	}
	rootFD := int(root.Fd())
	if err := unix.Mkdirat(rootFD, name, secretExecDirMode); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("create %s: %w", name, err)
	}
	childFD, err := unix.Openat(rootFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s without following links: %w", name, err)
	}
	defer func() { _ = unix.Close(childFD) }()
	var child unix.Stat_t
	if err := unix.Fstat(childFD, &child); err != nil {
		return fmt.Errorf("inspect %s: %w", name, err)
	}
	if child.Mode&unix.S_IFMT != unix.S_IFDIR || int64(child.Uid) != int64(secretExecEUID()) ||
		child.Mode&07777 != secretExecDirMode {
		return fmt.Errorf("secret child directory %s is not owner-private", name)
	}
	kind, err := secretExecFstatfs(childFD)
	if err != nil || kind != tmpfsMagic {
		return fmt.Errorf("secret child directory %s is not verified tmpfs", name)
	}
	for key, value := range values {
		if err := validateSecretSegment("KV field name", key); err != nil {
			return err
		}
		fd, err := unix.Openat(childFD, key, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, secretExecFileMode)
		if err != nil {
			return fmt.Errorf("create %s/%s: %w", name, key, err)
		}
		file := os.NewFile(uintptr(fd), key)
		if err := unix.Fchmod(fd, secretExecFileMode); err != nil {
			_ = file.Close()
			_ = unix.Unlinkat(childFD, key, 0)
			return fmt.Errorf("set private mode on %s/%s: %w", name, key, err)
		}
		_, writeErr := file.Write(value)
		closeErr := file.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			_ = unix.Unlinkat(childFD, key, 0)
			return fmt.Errorf("write %s/%s: %w", name, key, err)
		}
	}
	return nil
}
