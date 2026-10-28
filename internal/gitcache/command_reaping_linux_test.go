//go:build linux

package gitcache

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Use a private test process so enabling subreaping cannot change ownership in
// unrelated tests. Explicitly enable it on the old code too: cancellation must
// reap its adopted descendants, not depend on an external init to hide zombies.
func TestCLICommandReapsCanceledDescendants(t *testing.T) {
	if os.Getenv("OBERTH_REAP_REGRESSION") != "1" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestCLICommandReapsCanceledDescendants$", "-test.count=1")
		cmd.Env = append(os.Environ(), "OBERTH_REAP_REGRESSION=1", "GORACE=atexit_sleep_ms=0")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("isolated descendant regression: %v\n%s", err, output)
		}
		return
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		cause   error
		fixture string
	}{
		{context.Canceled, "reap-root"}, {context.DeadlineExceeded, "reap-root"},
		{context.Canceled, "hook-root"}, {context.DeadlineExceeded, "hook-root"},
	} {
		cause := test.cause
		t.Run(test.fixture+"/"+cause.Error(), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cache, err := New(Config{Root: filepath.Join(root, "cache"), GitBinary: executable,
				Env:      map[string]string{"ROOT_PID": filepath.Join(root, "root"), "CHILD_PID": filepath.Join(root, "child"), "LEAF_PID": filepath.Join(root, "leaf")},
				Upstream: func(string) (string, error) { return "/unused", nil }})
			if err != nil {
				t.Fatal(err)
			}
			// This direct child is in a different group. Its exit status belongs
			// to exec.Cmd.Wait, never the git descendant reaper.
			sentinel := exec.Command(executable, "internal-gitcache-fixture", "sleep")
			sentinel.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := sentinel.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sentinel.Process.Kill(); _ = sentinel.Wait() }()
			exited := exec.Command(executable, "internal-gitcache-fixture", "exit7")
			exited.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := exited.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if exited.ProcessState == nil {
					_ = exited.Wait()
				}
			}()
			var exitInfo unix.Siginfo
			if err := unix.Waitid(unix.P_PID, exited.Process.Pid, &exitInfo, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(context.Canceled)
			ready := &cliReadyWriter{ready: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				done <- cache.run(ctx, commandSpec{args: []string{"internal-gitcache-fixture", test.fixture}, stdout: ready, stderr: io.Discard})
			}()
			select {
			case <-ready.ready:
			case err := <-done:
				t.Fatalf("fixture exited before descendants were ready: %v", err)
			case <-time.After(10 * time.Second):
				t.Fatal("descendant fixture did not become ready")
			}
			pids := make([]int, 3)
			for i, name := range []string{"root", "child", "leaf"} {
				raw, err := os.ReadFile(filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
				pids[i], err = strconv.Atoi(strings.TrimSpace(string(raw)))
				if err != nil {
					t.Fatal(err)
				}
				group, err := unix.Getpgid(pids[i])
				if err != nil || group != pids[0] {
					t.Fatalf("fixture child not in command group: %d group=%d: %v", pids[i], group, err)
				}
			}
			// Also clean the intentionally failing baseline, so its zombies do
			// not survive the isolated regression process.
			defer func() {
				for _, pid := range pids[1:] {
					deadline := time.Now().Add(10 * time.Second)
					for {
						var status unix.WaitStatus
						waited, _ := unix.Wait4(pid, &status, unix.WNOHANG, nil)
						if waited == pid || errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
							break
						}
						if time.Now().After(deadline) {
							t.Errorf("could not clean baseline descendant %d", pid)
							break
						}
						time.Sleep(time.Millisecond)
					}
				}
			}()
			cancel(cause) // timeout cause is injected after readiness, not a race
			var stoppedErr error
			select {
			case err := <-done:
				stoppedErr = err
				if !errors.Is(err, cause) {
					t.Fatalf("lost cancellation cause: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("canceled command did not join")
			}
			for _, pid := range pids {
				if err := unix.Kill(pid, 0); !errors.Is(err, unix.ESRCH) {
					t.Errorf("command descendant %d remains alive or zombie: %v (operation: %v)", pid, err, stoppedErr)
				}
			}
			if err := sentinel.Process.Signal(syscall.Signal(0)); err != nil {
				t.Errorf("unrelated process was killed or reaped: %v", err)
			}
			var exitErr *exec.ExitError
			if err := exited.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
				t.Errorf("stole unrelated child's exit status: %v", err)
			}

		})
	}
}

type cliReadyWriter struct {
	once  sync.Once
	ready chan struct{}
	text  string
}

func (w *cliReadyWriter) Write(p []byte) (int, error) {
	w.text += string(p)
	if strings.Contains(w.text, "ready") {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}
