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

// Run preserves the command's streams and exit code and forwards termination
// signals to its process group. Measurement/write failures never change verdicts.
func Run(args []string, input io.Reader, output, stderr io.Writer) int {
	return run(args, input, output, stderr, "/sys/fs/cgroup", "/proc/self/cgroup", TerminationPath)
}

func run(args []string, input io.Reader, output, stderr io.Writer, mount, selfPath, termination string) int {
	if len(args) < 2 || args[0] != "--" {
		_, _ = fmt.Fprintln(stderr, "step-exec: expected -- command [args]")
		return 126
	}
	cmd := exec.CommandContext(context.Background(), args[1], args[2:]...) // #nosec G204 -- the admitted pipeline command runs without new privileges.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
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
				if s, ok := s.(syscall.Signal); ok {
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
	// O_TRUNC on kubelet's existing termination file; never create directories or
	// require a writable root filesystem. A killed wrapper leaves unknown data.
	if f, openErr := os.OpenFile(termination, os.O_WRONLY|os.O_TRUNC, 0); openErr == nil { // #nosec G304 G703 -- fixed kubelet termination bind file; tests inject a temporary file.
		_, _ = io.WriteString(f, Encode(usage))
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
