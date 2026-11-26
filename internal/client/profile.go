package client

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Profile is public installation metadata; credential paths are fixed by its directory.
type Profile struct {
	Uplink     bool   `json:"uplink,omitempty"`
	ServerURL  string `json:"server_url"`
	SSHURL     string `json:"ssh_url,omitempty"`
	AdminKey   string `json:"admin_key,omitempty"`
	AdminCert  string `json:"admin_certificate,omitempty"`
	CAFile     string `json:"ca_file,omitempty"`
	KnownHosts string `json:"known_hosts,omitempty"`
}

// ProfileDir returns one context's directory without accepting path components.
func ProfileDir(base, name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", failure("profile", nil)
	}
	return filepath.Join(base, name), nil
}

func owned(info fs.FileInfo, mask fs.FileMode) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid()) && info.Mode().Perm()&mask == 0
}

func directory(path string, private bool) (*os.Root, error) {
	info, err := os.Lstat(path)
	mask := fs.FileMode(022)
	if private {
		mask = 077
	}
	if err != nil || !info.IsDir() || !owned(info, mask) {
		operation := "read"
		if private {
			operation = "directory"
		}
		return nil, fileFailure(path, operation, err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fileFailure(path, "read", err)
	}
	return root, nil
}

// ReadPrivate refuses links, nonregular files, foreign ownership and public permissions.
func ReadPrivate(path string) ([]byte, error) { return readFile(path, true) }

// ReadMountedPrivate also accepts a Secret's confined kubelet projection.
// Ordinary files keep the owner-only rules of ReadPrivate.
func ReadMountedPrivate(path string) ([]byte, error) {
	if data, ok, err := readKubeletFile(path); ok {
		return data, err
	}
	return ReadPrivate(path)
}

// ReadPublic permits readable certificates and metadata, but refuses writable files.
func ReadPublic(path string) ([]byte, error) {
	if data, ok, err := readKubeletFile(path); ok {
		return data, err
	}
	return readFile(path, false)
}

func readFile(path string, private bool) ([]byte, error) {
	root, err := directory(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := openClientFile(root, filepath.Base(path))
	if err != nil {
		return nil, fileFailure(path, "read", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !owned(info, permissionMask(private)) || private && !privateMode(info) {
		operation := "read"
		if private {
			operation = "private"
		}
		return nil, fileFailure(path, operation, err)
	}
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, fileFailure(path, "read", err)
	}
	return data, nil
}

// WritePrivate atomically replaces an owned private regular file in a 0700 directory.
// The temporary file is synced before rename and removed on every failed write.
func WritePrivate(path string, data []byte) error { return writeClientFile(path, data, true) }

func writeClientFile(path string, data []byte, private bool) error {
	root, err := directory(filepath.Dir(path), private)
	if err != nil {
		return err
	}
	defer root.Close()
	return writeClientFileAt(root, filepath.Base(path), data, private)
}

func writeClientFileAt(root *os.Root, name string, data []byte, private bool) error {
	if err := fileTarget(root, name, private); err != nil {
		return err
	}
	file, err := replacePrivate(root, name, data)
	if file != nil {
		err = errors.Join(err, file.Close())
	}
	return err
}

// replacePrivate returns the published file open, pinning its identity until close.
func replacePrivate(root *os.Root, name string, data []byte) (*os.File, error) {
	path := filepath.Join(root.Name(), name)
	temp := ".oberth-" + rand.Text()
	file, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fileFailure(path, "write", err)
	}
	defer func() { _ = root.Remove(temp) }()
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	err = errors.Join(writeErr, syncErr)
	if err == nil {
		err = root.Rename(temp, name)
	}
	if err != nil {
		return nil, fileFailure(path, "write", errors.Join(err, file.Close()))
	}
	return file, syncDirectory(root)
}

func fileTarget(root *os.Root, name string, private bool) error {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || !owned(info, permissionMask(private)) || private && !privateMode(info) {
		return fileFailure(filepath.Join(root.Name(), name), "read", err)
	}
	return nil
}

func syncDirectory(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return fileFailure(root.Name(), "write", err)
	}
	defer file.Close()
	if err = file.Sync(); err != nil {
		return fileFailure(root.Name(), "write", err)
	}
	return nil
}

func loadProfile(base, name string) (Profile, string, error) {
	if _, err := os.Lstat(base); errors.Is(err, fs.ErrNotExist) && name == "" {
		return Profile{}, "", nil
	}
	baseRoot, err := directory(base, false)
	if err != nil {
		return Profile{}, "", err
	}
	defer baseRoot.Close()
	if name == "" {
		name, err = selectedProfile(base)
		if err != nil {
			return Profile{}, "", err
		}
		if name == "" {
			return Profile{}, "", nil
		}
	}
	dir, err := ProfileDir(base, name)
	if err != nil {
		return Profile{}, "", err
	}
	root, err := directory(dir, false)
	if err != nil {
		return Profile{}, "", err
	}
	_ = root.Close()
	p, err := readProfile(dir)
	return p, dir, err
}

func readProfile(dir string) (Profile, error) {
	data, err := readFile(filepath.Join(dir, "installation.json"), false)
	if err != nil {
		return Profile{}, err
	}
	var p Profile
	if json.Unmarshal(data, &p) != nil || strings.TrimSpace(p.ServerURL) == "" {
		return Profile{}, fileFailure(filepath.Join(dir, "installation.json"), "profile", err)
	}
	for _, path := range []string{p.AdminKey, p.AdminCert, p.CAFile} {
		if path != "" && (!filepath.IsAbs(path) || filepath.Dir(path) != dir) {
			return Profile{}, fileFailure(filepath.Join(dir, "installation.json"), "profile", nil)
		}
	}
	if p.KnownHosts != "" && !filepath.IsAbs(p.KnownHosts) {
		return Profile{}, fileFailure(filepath.Join(dir, "installation.json"), "profile", nil)
	}
	return p, nil
}

// os.Root resolves links itself; openat applies O_NOFOLLOW to the final component.
func openClientFile(root *os.Root, name string) (*os.File, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	fd, err := unix.Openat(int(directory.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(root.Name(), name)), nil
}

func privateMode(info fs.FileInfo) bool {
	mode := info.Mode().Perm()
	return mode == 0600 || mode == 0400
}

func selectedProfile(base string) (string, error) {
	path := filepath.Join(base, "current")
	name, err := os.Readlink(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fileFailure(path, "profile", err)
	}
	return name, nil
}
