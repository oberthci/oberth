//go:build linux

package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func newSecretExecTmpfsRoot(t *testing.T) (string, *os.File) {
	t.Helper()
	directory, err := os.MkdirTemp("/dev/shm", "oberth-exec-root-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	root, err := prepareSecretExecRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return directory, root
}

func TestSecretExecTightensExistingTmpfsRootBeforeFetch(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "ordinary-emptydir", mode: 0o755},
		{name: "sticky-tmpfs-root", mode: os.ModeSticky | 0o777},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory, root := newSecretExecTmpfsRoot(t)
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(directory, tc.mode); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(directory)
			if err != nil || before.Mode()&os.ModeSticky != tc.mode&os.ModeSticky {
				t.Fatalf("tmpfs fixture mode = %v, err=%v", before, err)
			}
			t.Setenv("VAULT_ADDR", "")
			t.Setenv(requireSwaplessEnv, "")
			err = runSecretStoreExec(t.Context(), []string{
				"--dir=" + directory, "--path=oberth/data/release/test", "--", "true",
			}, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "VAULT_ADDR is not set") {
				t.Fatalf("root was not prepared before credential setup: %v", err)
			}
			info, err := os.Lstat(directory)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != secretExecDirMode || info.Mode()&os.ModeSticky != 0 {
				t.Fatalf("existing tmpfs root mode = %v, want private 0700", info.Mode())
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("credential material appeared before fetch: entries=%v err=%v", entries, err)
			}
		})
	}
}

func TestSecretExecCreatesPrivateTmpfsRootBeforeFetch(t *testing.T) {
	parent, root := newSecretExecTmpfsRoot(t)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(parent, "new-mount-root")
	t.Setenv("VAULT_ADDR", "")
	t.Setenv(requireSwaplessEnv, "")
	err := runSecretStoreExec(t.Context(), []string{
		"--dir=" + directory, "--path=oberth/data/release/test", "--", "true",
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "VAULT_ADDR is not set") {
		t.Fatalf("root was not created before credential setup: %v", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != secretExecDirMode {
		t.Fatalf("new tmpfs root = %v, err=%v", info, err)
	}
}

func TestSecretExecRootRefusesSymlinkAndWrongOwnerBeforeFetch(t *testing.T) {
	for _, scenario := range []string{"symlink", "wrong-owner", "wrong-owner-sticky"} {
		t.Run(scenario, func(t *testing.T) {
			directory, root := newSecretExecTmpfsRoot(t)
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			mode := os.FileMode(0o755)
			if scenario == "wrong-owner-sticky" {
				mode = os.ModeSticky | 0o777
			}
			if err := os.Chmod(directory, mode); err != nil {
				t.Fatal(err)
			}
			candidate := directory
			if scenario == "symlink" {
				candidate = directory + "-link"
				if err := os.Symlink(directory, candidate); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(candidate) })
			} else {
				original := secretExecEUID
				secretExecEUID = func() int { return os.Geteuid() + 1 }
				t.Cleanup(func() { secretExecEUID = original })
			}
			t.Setenv("VAULT_ADDR", "")
			err := runSecretStoreExec(t.Context(), []string{
				"--dir=" + candidate, "--path=oberth/data/release/test", "--", "true",
			}, io.Discard, io.Discard)
			if err == nil || strings.Contains(err.Error(), "VAULT_ADDR") {
				t.Fatalf("unsafe root reached credential setup: %v", err)
			}
			if scenario != "symlink" {
				for _, field := range []string{"uid=", "gid=", "euid=", "mode=", "fstype="} {
					if !strings.Contains(err.Error(), field) {
						t.Fatalf("prefetch diagnostic missing %s: %v", field, err)
					}
				}
				if strings.Contains(err.Error(), directory) {
					t.Fatalf("prefetch diagnostic exposed a directory path: %v", err)
				}
			}
			info, err := os.Lstat(directory)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != mode.Perm() || info.Mode()&os.ModeSticky != mode&os.ModeSticky {
				t.Fatalf("rejected root was modified: mode=%v", info.Mode())
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected root received material: %v %v", entries, err)
			}
		})
	}
}

func TestSecretExecRootRefusesSetIDBeforeFetch(t *testing.T) {
	for _, tc := range []struct {
		name string
		bit  os.FileMode
	}{
		{name: "setuid", bit: os.ModeSetuid},
		{name: "setgid", bit: os.ModeSetgid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory, root := newSecretExecTmpfsRoot(t)
			if err := root.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(directory, tc.bit|0o700); err != nil {
				t.Fatal(err)
			}
			before, err := os.Lstat(directory)
			if err != nil || before.Mode()&tc.bit == 0 {
				t.Fatalf("set-ID fixture mode = %v, err=%v", before, err)
			}
			t.Setenv("VAULT_ADDR", "")
			err = runSecretStoreExec(t.Context(), []string{
				"--dir=" + directory, "--path=oberth/data/release/test", "--", "true",
			}, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "without setuid/setgid") || strings.Contains(err.Error(), "VAULT_ADDR") {
				t.Fatalf("set-ID root reached credential setup: %v", err)
			}
			for _, field := range []string{"uid=", "gid=", "euid=", "mode=", "fstype="} {
				if !strings.Contains(err.Error(), field) {
					t.Fatalf("prefetch diagnostic missing %s: %v", field, err)
				}
			}
			if strings.Contains(err.Error(), directory) {
				t.Fatalf("prefetch diagnostic exposed a directory path: %v", err)
			}
			after, err := os.Lstat(directory)
			if err != nil || after.Mode()&tc.bit == 0 {
				t.Fatalf("rejected set-ID root was modified: mode=%v, err=%v", after, err)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected root received material: %v %v", entries, err)
			}
		})
	}
}

func TestSecretExecRootChmodFailureReportsOnlyMountMetadata(t *testing.T) {
	directory, root := newSecretExecTmpfsRoot(t)
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, os.ModeSticky|0o777); err != nil {
		t.Fatal(err)
	}
	original := secretExecRootFchmod
	secretExecRootFchmod = func(_ int, _ uint32) error { return unix.EPERM }
	t.Cleanup(func() { secretExecRootFchmod = original })
	t.Setenv("VAULT_ADDR", "")
	err := runSecretStoreExec(t.Context(), []string{
		"--dir=" + directory, "--path=oberth/data/release/test", "--", "true",
	}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "tighten secret directory permissions") ||
		strings.Contains(err.Error(), "VAULT_ADDR") || strings.Contains(err.Error(), directory) {
		t.Fatalf("chmod refusal crossed the prefetch boundary or exposed a path: %v", err)
	}
	for _, field := range []string{"uid=", "gid=", "euid=", "mode=", "fstype="} {
		if !strings.Contains(err.Error(), field) {
			t.Fatalf("chmod refusal missing %s: %v", field, err)
		}
	}
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSticky == 0 {
		t.Fatalf("failed chmod changed the root: mode=%v, err=%v", info, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed chmod wrote material: entries=%v err=%v", entries, err)
	}
}

func TestSecretExecRootDiagnosticContainsExactMetadataOnly(t *testing.T) {
	directory, root := newSecretExecTmpfsRoot(t)
	var stat unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "private-entry-name"), []byte("private-entry-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := secretExecEUID
	secretExecEUID = func() int { return os.Geteuid() + 1 }
	t.Cleanup(func() { secretExecEUID = original })
	t.Setenv("VAULT_ADDR", "https://must-not-be-contacted.invalid")
	t.Setenv("VAULT_TOKEN", "private-environment-content")
	err := runSecretStoreExec(t.Context(), []string{
		"--dir=" + directory, "--path=oberth/data/release/private-path", "--", "true",
	}, io.Discard, io.Discard)
	want := fmt.Sprintf("prepare private secret directory: secret directory must be owned by the effective user without setuid/setgid (uid=%d gid=%d euid=%d mode=%#o fstype=%#x)",
		stat.Uid, stat.Gid, os.Geteuid()+1, stat.Mode&07777, tmpfsMagic)
	if err == nil || err.Error() != want {
		t.Fatalf("prefetch refusal = %v, want numeric-only diagnostic %q", err, want)
	}
}

func TestWriteSecretTreeFDUsesPrivateTmpfsAndRefusesAliases(t *testing.T) {
	directory, root := newSecretExecTmpfsRoot(t)
	if err := writeSecretTreeFD(root, "r2-upload", map[string][]byte{"token": []byte("synthetic-secret")}); err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(filepath.Join(directory, "r2-upload", "token"))
	if err != nil || string(secret) != "synthetic-secret" {
		t.Fatalf("pinned write = %q, %v", secret, err)
	}
	for _, relative := range []string{"r2-upload", "r2-upload/token"} {
		info, err := os.Lstat(filepath.Join(directory, relative))
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(secretExecDirMode)
		if relative == "r2-upload/token" {
			want = secretExecFileMode
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %o, want %o", relative, info.Mode().Perm(), want)
		}
	}
	if err := writeSecretTreeFD(root, "r2-upload", map[string][]byte{"token": []byte("replacement")}); err == nil {
		t.Fatal("duplicate credential file bypassed O_EXCL")
	}
	for _, bad := range []string{"..", "a/b", "a\\b"} {
		if err := writeSecretTreeFD(root, bad, map[string][]byte{"token": []byte("value")}); err == nil {
			t.Fatalf("unsafe local name %q accepted", bad)
		}
	}
	if err := writeSecretTreeFD(root, "field-test", map[string][]byte{"../escape": []byte("value")}); err == nil {
		t.Fatal("unsafe field name accepted")
	}
}

func TestWriteSecretTreeFDRefusesSymlinkAndPublicChild(t *testing.T) {
	directory, root := newSecretExecTmpfsRoot(t)
	outside, _ := newSecretExecTmpfsRoot(t)
	if err := os.Symlink(outside, filepath.Join(directory, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretTreeFD(root, "linked", map[string][]byte{"token": []byte("value")}); err == nil {
		t.Fatal("symlink child accepted")
	}
	if err := os.Mkdir(filepath.Join(directory, "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(directory, "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretTreeFD(root, "public", map[string][]byte{"token": []byte("value")}); err == nil {
		t.Fatal("public child accepted")
	}
	for _, path := range []string{filepath.Join(outside, "token"), filepath.Join(directory, "public", "token")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("rejected child received credential file %s: %v", path, err)
		}
	}
}
