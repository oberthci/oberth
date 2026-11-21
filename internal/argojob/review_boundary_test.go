package argojob

import (
	"context"
	"k8s.io/utils/clock"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestReviewSocketMode(t *testing.T) {
	r, c := testHoldRuntime(t)
	ready := &holdReadyWriter{ready: make(chan struct{})}
	done := make(chan int, 1)
	go func() {
		done <- r.dispatch(t.Context(), []string{"run", "--", "sh", "-c", "exit 7"}, HoldIO{In: strings.NewReader(""), Out: os.Stdout, Err: ready})
	}()
	timer := <-c.timers
	<-ready.ready
	info, err := os.Lstat(filepath.Join(r.root, "hold.sock"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Errorf("socket permissions: %v %v", info, err)
	}
	timer <- c.Now()
	if code := <-done; code != 7 {
		t.Fatal(code)
	}
}
func TestReviewFrameWriteDeadline(t *testing.T) {
	t.Parallel()
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	now := clock.RealClock{}.Now
	s := holdStream{conn: left, now: now}
	done := make(chan error, 1)
	go func() { done <- s.send(holdFrame{Type: "out", Data: []byte("blocked")}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked frame write succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("frame write deadline missing")
	}
}
func TestReviewSocketLossKillsBeforeBudget(t *testing.T) {
	t.Parallel()
	o, path, c := testHoldOwner(t)
	started := make(chan *inspection, 1)
	o.runtime.start = func(argv []string) (*inspection, error) {
		p, err := startInspection(argv)
		if err == nil {
			started <- p
		}
		return p, err
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeHoldFrame(conn, holdRequest{Version: 1, UID: "uid", Op: "exec", Argv: []string{"sh", "-c", "kill -STOP $$"}}); err != nil {
		t.Fatal(err)
	}
	p := <-started
	cleanupInspection(t, p)
	<-c.timers // Keep the budget unexpired.
	_ = conn.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	select {
	case <-p.done:
		if commandStatus(p.err) != 128+int(syscall.SIGKILL) {
			t.Fatal(p.err)
		}
	case <-ctx.Done():
		t.Fatal("closed socket did not kill request group before budget")
	}
}
