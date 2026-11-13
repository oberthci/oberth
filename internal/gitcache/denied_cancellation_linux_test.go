//go:build linux

package gitcache

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReviewerDeniedGroupSignalStillHonorsCancellation(t *testing.T) {
	const marker = "reviewer-denied-group"
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(os.Args, marker) {
		cmd := exec.Command("python3", "../../hack/testdata/git-group-permissions/exec.py", "13", exe, "-test.run=^TestReviewerDeniedGroupSignalStillHonorsCancellation$", "-test.count=1", "--", marker)
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("denied-group cancellation: %v\n%s", err, raw)
		}
		return
	}
	if err = prepareCLIWait(); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "internal-gitcache-fixture", "sleep")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	if err = unix.Kill(-child.Process.Pid, 0); !errors.Is(err, unix.EACCES) {
		child.Process.Kill()
		child.Wait()
		t.Fatal("denial fixture missing", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { waitCLICommand(ctx, child); close(done) }()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		// A positive-PID kill is permitted. Always clean the reproduction before failing.
		if err = child.Process.Kill(); err != nil {
			t.Fatal("fixture cleanup", err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("cleanup did not join")
		}
		t.Fatal("Git wait ignored its expired context while group SIGKILL was denied; it returned only after an external positive-PID kill")
	}
}
