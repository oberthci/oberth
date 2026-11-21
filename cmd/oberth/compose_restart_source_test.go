//go:build linux

// These tests exercise the Linux server and Git receive path.

package main

import (
	"context"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

// An accepted run's source lives in the server's Git cache under the run's keep
// ref and its admission in the server database, so the server running when the
// step starts serves it. Connections that first reach the departed server, as a
// Service does while its Pod is replaced, wait instead of failing the step.
func TestComposedAcceptedRunLoadsSourceFromTheReplacingServer(t *testing.T) {
	t.Parallel()
	f := composeFixture(t, composeBinary(t))
	commit := f.prepareCommit(t)
	cancel, done := f.run(t)
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	f.bindClient(t)
	if pushed, err := f.pushResult(t, "refs/heads/in-flight", "-o", "oberth.run=1"); err != nil {
		t.Fatal("push refused", pushed, err)
	}
	run := f.waitRun(t, func(r stateport.Run) bool { return r.Origin.WorkflowUID != "" })
	if run.CommitSHA == nil || *run.CommitSHA != commit {
		t.Fatal("accepted run lost its commit", run.RunSummary)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("first server did not stop")
	}
	f.restartChannels(t)
	_, restarted := f.run(t)
	f.checkStarting(t, restarted)
	f.waitStarted(t, restarted)
	var dials atomic.Int32
	departed := func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) <= 2 {
			return nil, &net.OpError{Op: "dial", Net: network, Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	f.deliverSourceThrough(t, run, departed)
	if dials.Load() < 3 {
		t.Fatal("load did not meet the departed server first", dials.Load())
	}
}
