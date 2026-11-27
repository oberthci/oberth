package argojob

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type holdOwner struct {
	mu              sync.Mutex
	closing         bool
	rerun           *holdRequest
	originalExit    int
	children        map[int]bool
	runtime         holdRuntime
	config          holdConfig
	since, deadline time.Time
	stopped, done   chan struct{}
}

func (r holdRuntime) hold(ctx context.Context, c holdConfig, code int, diagnostic io.Writer) int {
	since := r.now()
	o := &holdOwner{runtime: r, config: c, originalExit: code, since: since, deadline: since.Add(c.ttl), children: map[int]bool{}, stopped: make(chan struct{}), done: make(chan struct{})}
	monitorDone := make(chan os.Signal, 1)
	timer := r.after(c.ttl)
	go func() { monitorDone <- o.monitor(ctx, timer) }()
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(r.root, "hold.sock"), Net: "unix"})
	if err != nil {
		o.close()
		return holdDiagnostic(diagnostic, code, err)
	}
	defer listener.Close()
	if err = os.Chmod(listener.Addr().String(), 0600); err != nil {
		o.close()
		return holdDiagnostic(diagnostic, code, err)
	}
	go o.serve(listener)
	go holdFailureDiagnostic(c, code, since, diagnostic)
	<-o.done
	r.pendingSignal = <-monitorDone
	if o.rerun != nil {
		defer os.Remove(o.rerun.Stage)
		return r.rerunCommand(ctx, *o.rerun, code, diagnostic)
	}
	return code
}

func (o *holdOwner) monitor(ctx context.Context, timer <-chan time.Time) os.Signal {
	for {
		select {
		case <-timer:
		case s := <-o.runtime.signals:
			if routineHoldSignal(s) {
				continue
			}
			o.mu.Lock()
			if o.rerun != nil {
				o.mu.Unlock()
				return s
			}
			o.closeLocked()
			o.mu.Unlock()
			return nil
		case <-ctx.Done():
		case <-o.done:
			return nil
		}
		o.close()
		return nil
	}
}

func holdFailureDiagnostic(c holdConfig, code int, since time.Time, w io.Writer) {
	line := fmt.Sprintf("oberth-hold: command exited %d; held until about %s; release with run_hold_abandon\n", code, since.Add(c.ttl).UTC().Format(time.RFC3339))
	_, _ = io.WriteString(w, line)

}
func (o *holdOwner) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.closeLocked()
}
func (o *holdOwner) closeLocked() {
	if o.closing {
		return
	}
	o.stopLocked()
	close(o.done)
}
func (o *holdOwner) stopLocked() {
	o.closing = true
	close(o.stopped)
	for pid := range o.children {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}
func (o *holdOwner) serve(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go o.request(conn)
	}
}
func (o *holdOwner) request(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(o.deadline)
	stream := &holdStream{conn: conn, now: o.runtime.now}
	var r holdRequest
	if err := readHoldFrame(conn, &r); err != nil {
		_ = stream.send(holdFrame{Type: "refused", Reason: "invalid request"})
		return
	}
	if r.Op == "rerun" && (o.runtime.rerunPeer == nil || !o.runtime.rerunPeer(conn)) {
		_ = stream.send(holdFrame{Type: "refused", Reason: "rerun requires server exec"})
		return
	}
	cmd, frame, budget, release := o.admit(r)
	if cmd == nil {
		_ = stream.send(frame)
		if release {
			close(o.done)
		}
		return
	}
	o.execute(conn, stream, cmd, budget)
}
func validHoldRequest(r holdRequest) bool {
	if r.Version != holdProtocol || r.UID == "" || r.TimeoutMS < 0 || r.TimeoutMS > MaxExecTimeout.Milliseconds() || len(r.Argv) > 256 {
		return false
	}
	switch r.Op {
	case "status", "release":
		return true
	case "exec":
		return len(r.Argv) > 0 && r.Argv[0] != ""
	case "rerun":
		return r.Stage != "" && validRerunDigest(r.Digest)
	}
	return false
}

// Start and registration share the close mutex. Neither Wait nor socket I/O does.
func (o *holdOwner) admit(r holdRequest) (*inspection, holdFrame, <-chan time.Time, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !validHoldRequest(r) {
		return nil, holdFrame{Type: "refused", Reason: "invalid request"}, nil, false
	}
	if r.UID != o.config.uid || o.config.uid == "" {
		return nil, holdFrame{Type: "refused", Reason: "Pod UID mismatch"}, nil, false
	}
	remaining := o.deadline.Sub(o.runtime.now())
	if o.rerun != nil {
		return nil, rerunningReply(r.Op), nil, false
	}
	if o.closing || remaining <= 0 {
		return nil, holdFrame{Type: "closed"}, nil, false
	}
	if r.Op == "status" {
		return nil, holdFrame{Type: "held", HeldSince: o.since, TTL: int64(o.config.ttl / time.Second), RemainingMS: remaining.Milliseconds(), OriginalExit: o.originalExit}, nil, false
	}
	if r.Op == "rerun" {
		frame, end := o.admitRerun(r)
		return nil, frame, nil, end
	}
	if r.Op == "release" {
		o.stopLocked()
		return nil, holdFrame{Type: "closed"}, nil, true
	}
	child, err := o.runtime.start(r.Argv)
	if err != nil {
		return nil, holdFrame{Type: "start_failed", Reason: "command could not start"}, nil, false
	}
	o.children[child.cmd.Process.Pid] = true
	remaining = o.deadline.Sub(o.runtime.now())
	timeout := time.Duration(r.TimeoutMS) * time.Millisecond
	if timeout == 0 {
		timeout = DefaultExecTimeout
	}
	return child, holdFrame{}, o.runtime.after(min(timeout, remaining)), false
}

func rerunningReply(op string) holdFrame {
	switch op {
	case "status":
		return holdFrame{Type: "rerunning"}
	case "rerun":
		return holdFrame{Type: "already_rerun"}
	}
	return holdFrame{Type: "closed"}
}

func (o *holdOwner) admitRerun(r holdRequest) (holdFrame, bool) {
	if err := rerunRoot(o.runtime.checkoutRoot(), r.Stage); err != nil {
		return holdFrame{Type: "refused", Reason: "source root or stage is unsafe"}, false
	}
	o.rerun = &r
	o.stopLocked()
	return holdFrame{Type: "rerunning"}, true
}
