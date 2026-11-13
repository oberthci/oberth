//go:build linux

package gitcache

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

const groupSignalFilterMarker = "oberth-test-deny-group-signals"

// Re-exec the existing test payload, which is also allowed by the static Git
// launcher's execution policy. No interpreter, compiler or external helper is
// needed in the runtime image.
func groupSignalCommand(t *testing.T, refused unix.Errno, args ...string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := []string{"-test.run=^TestGroupSignalFilterEntrypoint$", "-test.count=1", "--", groupSignalFilterMarker, strconv.Itoa(int(refused))}
	return exec.Command(exe, append(argv, args...)...)
}

func TestGroupSignalFilterEntrypoint(t *testing.T) {
	marker := slices.Index(os.Args, groupSignalFilterMarker)
	if marker < 0 {
		return
	}
	if len(os.Args) <= marker+2 {
		t.Fatal("missing errno or target test arguments")
	}
	code, err := strconv.Atoi(os.Args[marker+1])
	if err != nil || (unix.Errno(code) != unix.EACCES && unix.Errno(code) != unix.EPERM) {
		t.Fatal("invalid permission errno", code, err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Match the Python fixture's kernel policy: refuse kill(negative PID,...)
	// only. Positive-PID and pidfd cleanup stay available. SYS_KILL is pinned by
	// the target Go architecture, so amd64 and arm64 need no runtime detection.
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 3, K: unix.SYS_KILL},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 16},
		{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, Jf: 1, K: 0x80000000},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(code)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	// prctl applies to the calling thread. Keep installation and exec on it;
	// exec collapses the process and all new Go threads inherit the filter.
	runtime.LockOSThread()
	if err = unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	errno := installGroupSignalFilter(&program)
	runtime.KeepAlive(filters)
	if errno != 0 {
		t.Fatal("install group-signal seccomp fixture", errno)
	}
	if err = unix.Exec(exe, append([]string{exe}, os.Args[marker+2:]...), os.Environ()); err != nil {
		t.Fatal("exec filtered test payload", err)
	}
}
