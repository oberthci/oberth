package argojob

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// These processes use private user, mount and PID namespaces, never a cluster.
func TestHoldNamespaceProcess(t *testing.T) {
	switch os.Getenv("OBERTH_HOLD_TEST_ROLE") {
	case "owner":
		namespaceOwner(t)
	case "peer":
		r := namespaceRuntime(t)
		if code := r.client(t.Context(), []string{"rerun", "--pod-uid", "uid", "--sha256", os.Getenv("OBERTH_HOLD_TEST_DIGEST")}, HoldIO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}); code != 0 {
			os.Exit(code)
		}
	case "mount":
		namespaceMount(t)
	case "small-stage":
		namespaceSmallStage(t)
	}
}

func namespaceRuntime(t *testing.T) holdRuntime {
	t.Helper()
	r, _ := testHoldRuntime(t)
	r.root = filepath.Join(os.Getenv("OBERTH_HOLD_TEST_ROOT"), "state")
	r.stageRoot = filepath.Join(os.Getenv("OBERTH_HOLD_TEST_ROOT"), holdStage)
	r.sourceRoot = filepath.Join(os.Getenv("OBERTH_HOLD_TEST_ROOT"), "work", "src")
	r.rerunPeer = serverRerunPeer
	return r
}

func privateMounts(t *testing.T) {
	t.Helper()
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
}

func namespaceOwner(t *testing.T) {
	privateMounts(t)
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("OBERTH_HOLD_TEST_FAKE_PROC") == "1" {
		spoofProc(t)
	}
	r := namespaceRuntime(t)
	r.sourceInfo, _ = os.Lstat(r.sourceRoot)
	o, _ := startTestHoldOwner(t, r)
	_, _ = fmt.Fprintln(os.Stdout, "READY")
	select {
	case <-o.done:
		// PID 1 must outlive the injected peer, including its clean exit
		// after reading the admission reply. Parent cleanup closes stdin.
		_, _ = io.Copy(io.Discard, os.Stdin)
	case <-time.After(20 * time.Second):
		t.Fatal("namespace hold did not end")
	}
}

func spoofProc(t *testing.T) {
	t.Helper()
	real := filepath.Join(os.Getenv("OBERTH_HOLD_TEST_ROOT"), "real-proc")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("/proc", real, "", syscall.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount("tmpfs", "/proc", "tmpfs", 0, "size=1m"); err != nil {
		t.Fatal(err)
	}
	// Test instrumentation can still inspect itself; numeric peer records are forged.
	for _, name := range []string{"self", "thread-self"} {
		if err := os.Symlink(filepath.Join(real, name), filepath.Join("/proc", name)); err != nil {
			t.Fatal(err)
		}
	}
	for pid := 1; pid < 64; pid++ {
		dir := fmt.Sprintf("/proc/%d", pid)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stat"), fmt.Appendf(nil, "%d (peer) S 0\n", pid), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFakeProcCannotGrantRerun(t *testing.T) {
	t.Setenv("OBERTH_HOLD_TEST_FAKE_PROC", "1")
	TestInspectionCannotInvokeRerunEvenWithoutSecrets(t)
}

func namespaceCommand(t *testing.T, role string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := holdTempDir(t)
	for _, dir := range []string{"state", "work/src", "volume", holdStage} {
		if err = os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), self, "-test.run=^TestHoldNamespaceProcess$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "OBERTH_HOLD_TEST_ROLE="+role, "OBERTH_HOLD_TEST_ROOT="+root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWPID,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}}, GidMappingsEnableSetgroups: false}
	return cmd
}

func startNamespaceOwner(t *testing.T) (*exec.Cmd, string) {
	t.Helper()
	cmd := namespaceCommand(t, "owner")
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		namespaceUnavailable(t, err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "READY\n" {
		t.Fatal("namespace owner unavailable", line, err, diagnostic.String())
	}
	root := strings.TrimPrefix(cmd.Env[len(cmd.Env)-1], "OBERTH_HOLD_TEST_ROOT=")
	return cmd, root
}

func TestInspectionCannotInvokeRerunEvenWithoutSecrets(t *testing.T) {
	_, root := startNamespaceOwner(t)
	bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	archive := filepath.Join(root, "snapshot")
	if err := os.WriteFile(archive, bundle, 0600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The inspection is a real child of the owner and stages its own snapshot.
	command := []string{"sh", "-c", `exec env OBERTH_HOLD_TEST_ROLE=peer OBERTH_HOLD_TEST_DIGEST="$1" "$2" -test.run=^TestHoldNamespaceProcess$ < "$3"`, "sh", digest, self, archive}
	socket := filepath.Join(root, "state", "hold.sock")
	_, next := sendHold(t, socket, holdRequest{Version: holdProtocol, Op: "exec", UID: "uid", Argv: command})
	var output string
	for {
		frame := next()
		output += string(frame.Data)
		if frame.Type != "out" && frame.Type != "err" && frame.Type != "exit" {
			t.Fatal("inspection reran its own snapshot", frame, output)
		}
		if frame.Type == "exit" {
			if frame.Code != 120 || !strings.Contains(output, "rerun requires server exec") {
				t.Fatal("inspection reran its own snapshot", frame, output)
			}
			break
		}
	}
	_, next = sendHold(t, socket, holdRequest{Version: holdProtocol, Op: "status", UID: "uid"})
	if frame := next(); frame.Type != "held" {
		t.Fatal("refused inspection released hold", frame)
	}
}

func TestServerExecParentZeroCanRerun(t *testing.T) {
	owner, root := startNamespaceOwner(t)
	bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The namespace's init is not this process's parent: the kernel reports 0.
	cmd := exec.CommandContext(t.Context(), "nsenter", "--target", strconv.Itoa(owner.Process.Pid), "--user", "--preserve-credentials", "--mount", "--pid", "--", self, "-test.run=^TestHoldNamespaceProcess$")
	cmd.Env = append(os.Environ(), "OBERTH_HOLD_TEST_ROLE=peer", "OBERTH_HOLD_TEST_ROOT="+root, "OBERTH_HOLD_TEST_DIGEST="+digest)
	cmd.Stdin = bytes.NewReader(bundle)
	if out, err := cmd.CombinedOutput(); err != nil || !bytes.Contains(out, []byte(`"t":"rerunning"`)) {
		t.Fatal("server exec with parent zero refused", string(out), err)
	}
}

func TestRerunUsesStateAcrossBindMount(t *testing.T) {
	cmd := namespaceCommand(t, "mount")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		namespaceUnavailable(t, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal("bind-mounted checkout rerun failed", out.String(), err)
	}
	t.Log(out.String())
}

func namespaceMount(t *testing.T) {
	privateMounts(t)
	r := namespaceRuntime(t)
	if err := syscall.Mount("tmpfs", r.root, "tmpfs", 0, "size=1m,mode=0700"); err != nil {
		t.Fatal(err)
	}
	volume := filepath.Join(os.Getenv("OBERTH_HOLD_TEST_ROOT"), "volume")
	if err := syscall.Mount("tmpfs", volume, "tmpfs", 0, "size=128m,mode=0700"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mount(volume, r.sourceRoot, "", syscall.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(r.sourceRoot), 0555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(filepath.Dir(r.sourceRoot), 0700) }()
	// Remove root's DAC override so the parent really is unwritable.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	caps := [2]unix.CapUserData{}
	if err := unix.Capset(&unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}, &caps[0]); err != nil {
		t.Fatal(err)
	}
	if f, err := os.CreateTemp(filepath.Dir(r.sourceRoot), "must-refuse"); err == nil {
		_ = f.Close()
		t.Fatal("source parent is writable")
	}
	r.sourceInfo, _ = os.Lstat(r.sourceRoot)
	state, _ := os.Stat(r.root)
	if r.sourceInfo.Sys().(*syscall.Stat_t).Dev == state.Sys().(*syscall.Stat_t).Dev {
		t.Fatal("fixture needs separate filesystems")
	}
	assertDiskStage(t, r.stageRoot)
	bundle, digest, commit := realisticRerunBundle(t)
	stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stage)
	if _, err = r.applyRerunSnapshot(t.Context(), stage, digest); err != nil {
		t.Fatal(err)
	}
	assertRealisticRerun(t, r.sourceRoot, commit)
	_, _ = io.WriteString(os.Stdout, "disk stage installed into bind-mounted checkout with unwritable parent and 1 MiB state\n")
}

func TestRerunRefusesReplacedRoot(t *testing.T) {
	for _, point := range []string{"admit", "install"} {
		t.Run(point, func(t *testing.T) { checkReplacedRoot(t, point) })
	}
}

func checkReplacedRoot(t *testing.T, point string) {
	t.Helper()
	r := rerunRuntime(t)
	bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stage)
	o, _, _ := testHoldOwner(t)
	o.runtime = r
	request := holdRequest{Version: holdProtocol, Op: "rerun", UID: "uid", Stage: stage, Digest: digest}
	if point == "install" {
		if _, frame, _, _ := o.admit(request); frame.Type != "rerunning" {
			t.Fatal(frame)
		}
	}
	if err = os.Rename(r.sourceRoot, r.sourceRoot+"-original"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(r.sourceRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if point == "admit" {
		if _, frame, _, _ := o.admit(request); frame.Type != "refused" {
			t.Fatal("replacement root admitted", frame)
		}
	} else if _, err = r.applyRerunSnapshot(t.Context(), stage, digest); err == nil {
		t.Fatal("replacement root installed")
	}
}
