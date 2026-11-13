//go:build linux

package gitcache

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Isolate a real kernel denial of negative-PID kill, without granting CAP_KILL
// or mocking signal results. Merely being outside a child's process group does
// not prohibit signaling same-UID members; confinement can prohibit that call.
func TestForgeProbeWithGroupSignalsRefused(t *testing.T) {
	const marker = "group-signals-refused"
	if !slices.Contains(os.Args, marker) {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		for _, refused := range []unix.Errno{unix.EACCES, unix.EPERM} {
			t.Run(refused.Error(), func(t *testing.T) {
				code := strconv.Itoa(int(refused))
				command := exec.Command("python3", "../../hack/testdata/git-group-permissions/exec.py", code, exe, "-test.run=^TestForgeProbeWithGroupSignalsRefused$", "-test.count=1", "--", marker, code)
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("isolated group-signal refusal: %v\n%s", err, output)
				}
			})
		}
		return
	}
	code, err := strconv.Atoi(os.Args[len(os.Args)-1])
	if err != nil {
		t.Fatal(err)
	}
	refused := unix.Errno(code)
	t.Chdir(t.TempDir())
	if err := prepareCLIWait(); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	leader := exec.Command(exe, "internal-gitcache-fixture", "exit7")
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = leader.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = leader.Wait() }()
	var info unix.Siginfo
	if err = unix.Waitid(unix.P_PID, leader.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		t.Fatal(err)
	}
	if err = unix.Kill(-leader.Process.Pid, unix.SIGKILL); !errors.Is(err, refused) {
		t.Fatal("fixture did not refuse group signals", err)
	}
	if err = reapCLIGroup(leader.Process.Pid, time.Now().Add(time.Second)); err != nil {
		t.Fatal("an exited probe needed no group signal", err)
	}
	root := t.TempDir()
	bare := filepath.Join(root, "upstream.git")
	runGit(t, "", "init", "--quiet", "--bare", bare)
	cache := newTestCache(t, bare)
	heads, err := cache.LsRemoteHeads(t.Context(), "example")
	if err != nil || heads != 0 {
		t.Fatal("successful forge result replaced by cleanup refusal", heads, err)
	}
	missing := newTestCache(t, filepath.Join(root, "missing.git"))
	_, err = missing.LsRemoteHeads(t.Context(), "example")
	if err == nil || strings.Contains(err.Error(), "stop git process group") || !strings.Contains(err.Error(), "git command failed") {
		t.Fatal("forge failure lost its own result", err)
	}

	// Keep a living descendant behind an exited leader. This refusal must
	// still fail cleanup; the probe cannot release its lock over a live child.
	leafFile := filepath.Join(root, "leaf.pid")
	child := exec.Command(exe, "internal-gitcache-fixture", "reap-child")
	child.Env = append(os.Environ(), "CHILD_PID="+filepath.Join(root, "child.pid"), "LEAF_PID="+leafFile)
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	var leaf int
	defer func() {
		if leaf != 0 {
			_ = unix.Kill(leaf, unix.SIGKILL)
		}
		_ = child.Process.Kill()
		_ = reapCLIGroup(child.Process.Pid, time.Now().Add(5*time.Second))
		_ = child.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for leaf == 0 {
		raw, readErr := os.ReadFile(leafFile)
		if readErr == nil {
			leaf, err = strconv.Atoi(string(raw))
			if err != nil {
				t.Fatal(err)
			}
		} else if !os.IsNotExist(readErr) || time.Now().After(deadline) {
			t.Fatal("descendant did not start", readErr)
		}
		time.Sleep(time.Millisecond)
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err = unix.Waitid(unix.P_PID, child.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		t.Fatal(err)
	}
	if err = reapCLIGroup(child.Process.Pid, time.Now().Add(time.Second)); !errors.Is(err, refused) {
		t.Fatal("living descendant cleanup refusal was hidden", err)
	}
}
