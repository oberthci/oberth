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
	children        map[int]bool
	runtime         holdRuntime
	config          holdConfig
	since, deadline time.Time
	stopped, done   chan struct{}
}

func (r holdRuntime) hold(ctx context.Context, c holdConfig, code int, diagnostic io.Writer) int {
	since := r.now()
	o := &holdOwner{runtime: r, config: c, since: since, deadline: since.Add(c.ttl), children: map[int]bool{}, stopped: make(chan struct{}), done: make(chan struct{})}
	timer := r.after(c.ttl)
	go func() {
		for {
			select {
			case <-timer:
			case s := <-r.signals:
				if routineHoldSignal(s) {
					continue
				}
			case <-ctx.Done():
			case <-o.done:
				return
			}
			o.close()
			return
		}
	}()
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
	return code
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
	return r.Version == 1 && r.UID != "" && r.TimeoutMS >= 0 && r.TimeoutMS <= MaxExecTimeout.Milliseconds() && len(r.Argv) <= 256 && (r.Op == "status" || r.Op == "release" || r.Op == "exec" && len(r.Argv) > 0 && r.Argv[0] != "")
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
	if o.closing || remaining <= 0 {
		return nil, holdFrame{Type: "closed"}, nil, false
	}
	if r.Op == "status" {
		return nil, holdFrame{Type: "held", HeldSince: o.since, TTL: int64(o.config.ttl / time.Second), RemainingMS: remaining.Milliseconds()}, nil, false
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
