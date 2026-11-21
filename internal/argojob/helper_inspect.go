package argojob

import (
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type inspection struct {
	cmd         *exec.Cmd
	out, detail *os.File
	done        chan struct{}
	err         error
}

func startInspection(argv []string) (*inspection, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, ow, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer ow.Close()
	detail, dw, err := os.Pipe()
	if err != nil {
		_ = out.Close()
		return nil, err
	}
	defer dw.Close()
	cmd.Stdout, cmd.Stderr = ow, dw
	if err = cmd.Start(); err != nil {
		_ = out.Close()
		_ = detail.Close()
		return nil, err
	}
	child := &inspection{cmd: cmd, out: out, detail: detail, done: make(chan struct{})}
	go func() { child.err = cmd.Wait(); close(child.done) }()
	return child, nil
}
func (o *holdOwner) execute(conn net.Conn, s *holdStream, child *inspection, timer <-chan time.Time) {
	pid := child.cmd.Process.Pid
	defer func() { o.mu.Lock(); delete(o.children, pid); o.mu.Unlock(); _ = syscall.Kill(-pid, syscall.SIGKILL) }()
	defer child.out.Close()
	defer child.detail.Close()
	lost := make(chan struct{})
	var loss sync.Once
	lose := func() { loss.Do(func() { close(lost) }) }
	drained := make(chan struct{})
	go func() {
		var both sync.WaitGroup
		both.Add(2)
		go func() { defer both.Done(); drainHoldOutput(child.out, s, "out", lose) }()
		go func() { defer both.Done(); drainHoldOutput(child.detail, s, "err", lose) }()
		both.Wait()
		close(drained)
	}()
	go func() {
		var b [1]byte
		_, _ = conn.Read(b[:])
		if err := s.send(holdFrame{Type: "out"}); err != nil {
			lose()
		}
	}()
	select {
	case <-child.done:
		select {
		case <-drained:
			_ = s.send(o.inspectionEnd(holdFrame{Type: "exit", Code: commandStatus(child.err)}))
		case <-timer:
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = s.send(o.inspectionEnd(holdFrame{Type: "timeout"}))
		case <-o.stopped:
			_ = s.send(holdFrame{Type: "closed"})
		case <-lost:
		}
	case <-timer:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = s.send(o.inspectionEnd(holdFrame{Type: "timeout"}))
	case <-o.stopped:
		_ = s.send(holdFrame{Type: "closed"})
	case <-lost:
	}
}
func drainHoldOutput(r io.Reader, s *holdStream, kind string, lose func()) {
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if sendErr := s.output(kind, buf[:n]); sendErr != nil {
				lose()
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (o *holdOwner) inspectionEnd(f holdFrame) holdFrame {
	select {
	case <-o.stopped:
		return holdFrame{Type: "closed"}
	default:
		return f
	}
}
