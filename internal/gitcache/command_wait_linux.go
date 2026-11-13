//go:build linux

package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var cliReaper struct {
	sync.Once
	err error
}

// Subreaper adoption is process-wide, as it already is when the server is PID 1.
// Reaping is strictly per command group: never wait for an arbitrary child, which
// could steal another exec.Cmd's exit status. No capabilities are required.
func prepareCLIWait() error {
	cliReaper.Do(func() {
		if _, err := readCLIProcess(os.Getpid()); err != nil {
			cliReaper.err = fmt.Errorf("git descendant reaping requires readable procfs: %w", err)
			return
		}
		fd, err := unix.PidfdOpen(os.Getpid(), 0)
		if err != nil {
			cliReaper.err = fmt.Errorf("git descendant reaping requires pidfds: %w", err)
			return
		}
		// Prove individual signaling is available before starting a child.
		// It is also the ownership-safe fallback when group signals are denied.
		if err := unix.PidfdSendSignal(fd, 0, nil, 0); err != nil {
			_ = unix.Close(fd)
			cliReaper.err = fmt.Errorf("git descendant cleanup requires pidfd signaling: %w", err)
			return
		}
		var info unix.Siginfo
		probeErr := unix.Waitid(unix.P_PIDFD, fd, &info, unix.WEXITED|unix.WNOHANG, nil)
		_ = unix.Close(fd)
		if !errors.Is(probeErr, unix.ECHILD) {
			cliReaper.err = errors.New("git descendant reaping requires pidfd wait support")
			if probeErr != nil {
				cliReaper.err = fmt.Errorf("%w: %w", cliReaper.err, probeErr)
			}
			return
		}
		cliReaper.err = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
	})
	return cliReaper.err
}

// Waitid observes but does not reap the leader. Until cmd.Wait, its PID/PGID
// cannot be reused, so every group signal targets this command, even if its
// leader exited concurrently with cancellation.
func waitCLICommand(ctx context.Context, cmd *exec.Cmd) (error, error) {
	leader := make(chan error, 1)
	go func() {
		var info unix.Siginfo
		for {
			err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if !errors.Is(err, unix.EINTR) {
				leader <- err
				return
			}
		}
	}()
	stopped := false
	var observationErr, signalErr error
	select {
	case observationErr = <-leader:
	case <-ctx.Done():
		stopped = true
		// The leader has not been reaped, including when the exit notification
		// and cancellation are simultaneously ready.
		signalErr = stopCLIGroup(cmd.Process.Pid)
		if signalErr != nil {
			// The unreaped direct child still pins its own PID, so a positive
			// PID signal cannot target a replacement even if pidfd signaling
			// failed for another group member. Never discard a stop failure.
			err := unix.Kill(cmd.Process.Pid, unix.SIGKILL)
			if err != nil && !errors.Is(err, unix.ESRCH) {
				signalErr = errors.Join(signalErr, err)
			}
		}
		observationErr = <-leader
	}
	if observationErr != nil {
		// Without the held leader, do not risk signaling a reused group ID.
		waitErr := errors.Join(fmt.Errorf("observe git process: %w", observationErr), cmd.Wait())
		if stopped {
			return waitErr, errors.Join(context.Cause(ctx), signalErr, observationErr)
		}
		return waitErr, nil
	}
	// Normal exits must also join unexpected background descendants before
	// exposing output or releasing the repository lock.
	cleanupErr := reapCLIGroup(cmd.Process.Pid, time.Now().Add(5*time.Second))
	waitErr := errors.Join(cleanupErr, cmd.Wait())
	if stopped {
		return waitErr, errors.Join(context.Cause(ctx), signalErr, cleanupErr)
	}
	return waitErr, nil
}

type cliProcess struct {
	pid, parent, group int
	state              byte
}

func readCLIProcess(pid int) (cliProcess, error) {
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return cliProcess{}, err
	}
	// comm is parenthesized and can itself contain spaces or ')'. All fields
	// after its final ')' have fixed positions and contain no parentheses.
	end := strings.LastIndexByte(string(body), ')')
	if end < 0 {
		return cliProcess{}, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(body[end+1:]))
	if len(fields) < 3 || len(fields[0]) != 1 {
		return cliProcess{}, errors.New("invalid process stat fields")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return cliProcess{}, err
	}
	group, err := strconv.Atoi(fields[2])
	return cliProcess{pid: pid, parent: parent, group: group, state: fields[0][0]}, err
}

func reapCLIGroup(group int, deadline time.Time) error {
	for {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return fmt.Errorf("inspect git descendants: %w", err)
		}
		remaining, live := false, false
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid == group {
				continue
			}
			currentGroup, err := unix.Getpgid(pid)
			if errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect process group for git cleanup: %w", err)
			}
			if currentGroup != group {
				continue
			}
			process, err := readCLIProcess(pid)
			if os.IsNotExist(err) || errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect process %d for git cleanup: %w", pid, err)
			}
			if process.group != group {
				continue
			}
			remaining = true
			if process.state != 'Z' && process.state != 'X' {
				live = true
			}
			if process.parent == os.Getpid() && process.state == 'Z' {
				if err := reapCLIChild(pid, group); err != nil {
					return fmt.Errorf("reap git descendant: %w", err)
				}
			}
		}
		if !remaining {
			return nil
		}
		// The held, exited leader pins the group ID. Only living descendants
		// need a signal; an empty group or zombies cannot fork. Some container
		// policies refuse group signals even when the forge probe has exited
		// successfully. Do not make that unnecessary syscall a probe failure.
		if live {
			if err := stopCLIGroup(group); err != nil {
				return err
			}
		}
		if time.Now().After(deadline) {
			return errors.New("git descendant cleanup exceeded five seconds")
		}
		time.Sleep(time.Millisecond)
	}
}

func reapCLIChild(pid, group int) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	// Recheck after opening the pidfd. A zombie cannot change group; a stale
	// /proc snapshot or PID reuse must never steal an unrelated child's wait.
	process, err := readCLIProcess(pid)
	if os.IsNotExist(err) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	if process.parent != os.Getpid() || process.group != group || process.state != 'Z' {
		return nil
	}
	var info unix.Siginfo
	for {
		err = unix.Waitid(unix.P_PIDFD, fd, &info, unix.WEXITED|unix.WNOHANG, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if errors.Is(err, unix.ECHILD) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

// The unreaped leader pins group throughout this call. Confinement may deny
// negative-PID kill while permitting individual signals. Open a pidfd, recheck
// membership, then signal that exact process: a recycled /proc PID cannot kill
// an unrelated process. Cleanup repeats this scan for forks already in flight.
func stopCLIGroup(group int) error {
	err := unix.Kill(-group, unix.SIGKILL)
	if err == nil || errors.Is(err, unix.ESRCH) {
		return nil
	}
	if !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EPERM) {
		return fmt.Errorf("stop git process group: %w", err)
	}
	entries, readErr := os.ReadDir("/proc")
	if readErr != nil {
		return errors.Join(err, fmt.Errorf("inspect git group for individual signals: %w", readErr))
	}
	var failures error
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			continue
		}
		process, inspectErr := readCLIProcess(pid)
		if os.IsNotExist(inspectErr) || errors.Is(inspectErr, unix.ESRCH) {
			continue
		}
		if inspectErr != nil {
			failures = errors.Join(failures, inspectErr)
			continue
		}
		if process.group != group || process.state == 'Z' || process.state == 'X' {
			continue
		}
		if signalErr := stopCLIProcess(pid, group); signalErr != nil {
			failures = errors.Join(failures, signalErr)
		}
	}
	if failures != nil {
		return fmt.Errorf("stop git group members: %w", failures)
	}
	return nil
}

func stopCLIProcess(pid, group int) error {
	fd, err := unix.PidfdOpen(pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	process, err := readCLIProcess(pid)
	if os.IsNotExist(err) || errors.Is(err, unix.ESRCH) {
		return nil
	}
	if err != nil {
		return err
	}
	if process.group != group || process.state == 'Z' || process.state == 'X' {
		return nil
	}
	for {
		err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}
