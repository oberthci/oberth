//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"context"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
)

func TestSecretHelperStaticWithoutShellOrLibc(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	binary := filepath.Join(dir, "oberth")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	// The release build flags, without stripping, as hack/release-bytes.sh uses them.
	build := exec.CommandContext(ctx, "go", "build", "-p=2", "-buildvcs=false", "-trimpath", "-ldflags=-buildid=", "-o", binary, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatal("static program build failed", err, string(raw))
	}
	info, err := os.Stat(binary)
	if err != nil || info.Size() > argojob.SecretHelperVolumeBytes/2 {
		t.Fatal("supplied program exceeds half its volume limit", info, err)
	}
	t.Log("supplied program bytes:", info.Size(), "volume limit:", argojob.SecretHelperVolumeBytes)
	file, err := elf.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP || program.Type == elf.PT_DYNAMIC {
			t.Fatal("program depends on a dynamic loader")
		}
	}
	libs, err := file.ImportedLibraries()
	if err != nil || len(libs) != 0 {
		t.Fatal("program depends on shared libraries", libs, err)
	}
	t.Log("CGO_ENABLED=0 product executable:", file.Machine, "no interpreter, dynamic segment or imported libraries")
	if os.Geteuid() != 0 {
		t.Log("empty-root execution requires root; static ELF verified")
		return
	}
	checkSecretHelperEmptyRoot(t, binary)
}

func checkSecretHelperEmptyRoot(t *testing.T, binary string) {
	t.Helper()
	root := t.TempDir()
	destination := filepath.Join(root, "run/oberth/bin")
	if err := os.MkdirAll(destination, 0755); err != nil {
		t.Fatal(err)
	}
	_, detail, code := heldExecutable(t, binary, []string{"internal-install", "--to", filepath.Join(destination, "oberth")})
	if code != 0 {
		t.Fatal("static copy failed", detail)
	}
	if err := os.Chmod(root, 0555); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/run/oberth/bin/oberth", "secretstore", "exec", "--", "unused")
	command.Dir, command.Env = "/", []string{"HOME=", "XDG_CONFIG_HOME="}
	command.SysProcAttr = &syscall.SysProcAttr{Chroot: root, Credential: &syscall.Credential{Uid: 65532, Gid: 65532}}
	raw, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(raw), "OBERTH_SECRETSTORE_CONFIG") {
		t.Fatal("secret exec did not start in an empty, unwritable root", err, string(raw))
	}
	t.Log("copied helper reached secret exec as UID 65532 in a root containing only the program; no shell, libc or writable root")
}
