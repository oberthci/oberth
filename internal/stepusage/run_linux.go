//go:build linux

package stepusage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// Run preserves the command's streams and exit code and forwards catchable
// signals to its process group. Like emissary, it ignores SIGCHLD (wait handles
// child exit) and SIGURG (Go runtime preemption). SIGKILL/SIGSTOP cannot be caught.
// A start failure returns shell-style 127, not emissary's executor Error; a
// signaled child returns 128+signal. Measurement/write failures never change
// verdicts, and a step-written termination message takes priority over usage.
func Run(args []string, input io.Reader, output, stderr io.Writer) int {
	return run(args, input, output, stderr, "/sys/fs/cgroup", "/proc/self/cgroup", TerminationPath)
}

func run(args []string, input io.Reader, output, stderr io.Writer, mount, selfPath, termination string) int {
	if len(args) < 2 || args[0] != "--" {
		_, _ = fmt.Fprintln(stderr, "step-exec: expected -- command [args]")
		return 126
	}
	cmd := exec.CommandContext(context.Background(), args[1], args[2:]...) // #nosec G204 G702 -- intentionally executes the admitted pipeline command without new privileges.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	signals := make(chan os.Signal, 64)
	signal.Notify(signals, forwardableSignals()...)
	defer signal.Stop(signals)
	if err := cmd.Start(); err != nil {
		_, _ = fmt.Fprintf(stderr, "step-exec: start: %v\n", err)
		return 127
	}
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		for {
			select {
			case s := <-signals:
				if s, ok := s.(syscall.Signal); ok && s != syscall.SIGCHLD && s != syscall.SIGURG {
					_ = syscall.Kill(-cmd.Process.Pid, s)
				}
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)
	<-exited
	self, _ := readBounded(selfPath)
	usage := ReadCgroupV2(mount, self)
	if cmd.ProcessState != nil {
		if r, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage); ok && r.Maxrss >= 0 && r.Maxrss <= math.MaxInt64/1024 {
			rss := r.Maxrss * 1024
			usage.MaxRSSBytes = &rss
		}
	}
	// Preserve any step-written termination message. Use the existing kubelet
	// file without truncation or creation; only an empty, readable file accepts
	// telemetry. A killed wrapper never writes a summary and leaves usage unknown.
	if f, openErr := os.OpenFile(termination, os.O_RDWR, 0); openErr == nil { // #nosec G304 G703 -- fixed kubelet termination bind file; tests inject a temporary file.
		var first [1]byte
		if n, readErr := f.Read(first[:]); n == 0 && readErr == io.EOF {
			_, _ = io.WriteString(f, Encode(usage))
		}
		_ = f.Close()
	}
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exit.ExitCode()
	}
	return 1
}

// Subscribe only to application-forwardable Linux signals, excluding child exit
// and Go preemption at the source so their bursts cannot fill the Notify queue.
// SIGKILL/SIGSTOP are uncatchable; 32/33 are reserved by the Linux threading ABI.
func forwardableSignals() []os.Signal {
	signals := make([]os.Signal, 0, 64)
	for s := syscall.Signal(1); s <= 64; s++ {
		if s != syscall.SIGKILL && s != syscall.SIGSTOP && s != syscall.SIGCHLD && s != syscall.SIGURG && s != 32 && s != 33 {
			signals = append(signals, s)
		}
	}
	return signals
}
