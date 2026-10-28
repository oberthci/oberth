package gitcache

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// The same test executable supplies native process-tree
// fixtures. None needs a shell, setsid utility, cp, or sleep in the runtime.
func runNativeFixture(arguments []string) int {
	if len(arguments) != 1 {
		return 2
	}
	executable, err := os.Executable()
	if err != nil {
		return 2
	}
	spawn := func(mode string) *exec.Cmd {
		child := exec.Command(executable, "internal-gitcache-fixture", mode)
		child.Env = os.Environ()
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		return child
	}
	switch arguments[0] {
	case "sleep":
		time.Sleep(time.Hour)
		return 0
	case "exit7":
		return 7
	case "hook-root":
		if err := os.WriteFile(os.Getenv("ROOT_PID"), []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
			return 2
		}
		cache := newNativeHookCache()
		cache.gitBinary = executable
		for _, key := range []string{"ROOT_PID", "CHILD_PID", "LEAF_PID", "GORACE"} {
			cache.env[key] = os.Getenv(key)
		}
		if err := cache.run(context.Background(), commandSpec{args: []string{"internal-gitcache-fixture", "reap-child"}, stdout: os.Stdout, stderr: os.Stderr}); err != nil {
			return 2
		}
		return 0
	case "reap-root":
		if err := os.WriteFile(os.Getenv("ROOT_PID"), []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
			return 2
		}
		child := spawn("reap-child")
		if err := child.Start(); err != nil {
			return 2
		}
		_ = child.Wait()
		return 0
	case "reap-child":
		if err := os.WriteFile(os.Getenv("CHILD_PID"), []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
			return 2
		}
		leaf := spawn("sleep")
		if err := leaf.Start(); err != nil {
			return 2
		}
		if err := os.WriteFile(os.Getenv("LEAF_PID"), []byte(fmt.Sprint(leaf.Process.Pid)), 0600); err != nil {
			return 2
		}
		_, _ = fmt.Fprintln(os.Stdout, "ready")
		_ = leaf.Wait()
		return 0
	case "hold-pipe":
		child := spawn("escaped")
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := child.Start(); err != nil {
			return 2
		}
		ready, err := os.Open(os.Getenv("ESCAPED_READY"))
		if err != nil {
			return 2
		}
		defer ready.Close()
		var message string
		if _, err := fmt.Fscanln(ready, &message); err != nil || message != "ready" {
			return 2
		}
		return 0 // Deliberately leave the separate group to the test's cleanup.
	case "escaped":
		if err := os.WriteFile(os.Getenv("ESCAPED_PID"), []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
			return 2
		}
		ready, err := os.OpenFile(os.Getenv("ESCAPED_READY"), os.O_WRONLY, 0600)
		if err != nil {
			return 2
		}
		_, err = fmt.Fprintln(ready, "ready")
		_ = ready.Close()
		if err != nil {
			return 2
		}
		time.Sleep(time.Hour)
		return 0
	}
	return 2
}

func nativeFixtureInvocation() (int, bool) {
	if len(os.Args) > 1 && os.Args[1] == "internal-gitcache-fixture" {
		return runNativeFixture(os.Args[2:]), true
	}
	return 0, false
}
