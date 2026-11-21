package argojob

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestBExecAdmissionBarriers(t *testing.T) {
	t.Parallel()
	for _, release := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry", true: "release"}[release], func(t *testing.T) {
			checkExecAdmissionBarriers(t, release)
		})
	}
}

func checkExecAdmissionBarriers(t *testing.T, release bool) {
	t.Helper()
	o, _, clock := testHoldOwner(t)
	validated, started, registered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	allowStart, allowRegister, allowReturn := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var process *os.Process
	o.runtime.start = func(argv []string) (*inspection, error) {
		if o.mu.TryLock() {
			o.mu.Unlock()
			t.Error("start is outside close mutex")
		}
		close(validated)
		<-allowStart
		child, err := startInspection(argv)
		if err == nil {
			process = child.cmd.Process
		}
		close(started)
		<-allowRegister
		return child, err
	}
	reads := 0
	o.runtime.now = func() time.Time {
		reads++
		if reads == 2 {
			if o.mu.TryLock() {
				o.mu.Unlock()
				t.Error("register is outside close mutex")
			}
			if process == nil || !o.children[process.Pid] {
				t.Error("budget computed before child registration")
			}
			close(registered)
			<-allowReturn
		}
		return clock.Now()
	}
	childResult := make(chan *inspection, 1)
	go func() {
		child, _, _, _ := o.admit(holdRequest{Version: 1, UID: "uid", Op: "exec", Argv: []string{"sh", "-c", "exec cat /dev/zero >/dev/null"}})
		childResult <- child
	}()
	<-validated
	closeBegan, closed := make(chan struct{}), make(chan struct{})
	go func() {
		close(closeBegan)
		if release {
			_, _, _, _ = o.admit(holdRequest{Version: 1, UID: "uid", Op: "release"})
		} else {
			o.close()
		}
		close(closed)
	}()
	<-closeBegan
	close(allowStart)
	<-started
	close(allowRegister)
	<-registered
	close(allowReturn)
	child := <-childResult
	<-closed
	if child == nil {
		t.Fatal("admitted child absent")
	}
	budget, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	select {
	case <-child.done:
	case <-budget.Done():
		_ = syscall.Kill(-child.cmd.Process.Pid, syscall.SIGKILL)
		<-child.done
		t.Fatal("close did not kill the admitted group")
	}
	err := child.err
	_ = child.out.Close()
	_ = child.detail.Close()
	if commandStatus(err) != 128+int(syscall.SIGKILL) {
		t.Fatal("close did not kill registered group", err)
	}
}
