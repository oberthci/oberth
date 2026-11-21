package argojob

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestDelta2HeldWorkspaceMakesNextParentAttemptPass(t *testing.T) {
	first, _ := testHoldRuntime(t)
	result := filepath.Join(first.root, "result")
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	var inspections atomic.Int32
	first.start = func(argv []string) (*inspection, error) { inspections.Add(1); return startInspection(argv) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ended := make(chan int, 1)
	script := `printf original > "$1"; (while [ ! -S "$2/hold.sock" ]; do :; done; printf background > "$1"; printf done > "$2/done") >/dev/null 2>&1 & printf '%s' "$!" > "$2/pid"; exit 7`
	go func() {
		ended <- first.run(ctx, []string{"sh", "-c", script, "sh", result, first.root}, HoldIO{In: null, Out: null, Err: null})
	}()
	defer func() {
		if b, e := os.ReadFile(filepath.Join(first.root, "pid")); e == nil {
			if pid, e := strconv.Atoi(string(b)); e == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	limit := time.NewTimer(2 * time.Second)
	defer limit.Stop()
waiting:
	for {
		select {
		case <-tick.C:
			if _, err = os.Stat(filepath.Join(first.root, "done")); err == nil {
				break waiting
			}
		case <-limit.C:
			t.Fatal("descendant did not write during hold")
		}
	}
	releaseProbeHold(t, first, ended)
	second, _ := testHoldRuntime(t)
	second.start = first.start
	command := []string{"sh", "-c", `test "$(cat "$1")" = background`, "sh", result}
	if code := second.run(t.Context(), command, HoldIO{In: null, Out: null, Err: io.Discard}); code != 0 {
		t.Fatal("second parent attempt failed", code)
	}
	if inspections.Load() != 0 {
		t.Fatal("an inspection was dispatched")
	}
	if err = os.WriteFile(result, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = exec.Command(command[0], command[1:]...).Run(); err == nil {
		t.Fatal("unchanged workspace unexpectedly passed")
	}
	t.Log("held leaf returned 7; original descendant changed shared output without exec; next parent attempt returned 0; unchanged-output control returned nonzero")
}
