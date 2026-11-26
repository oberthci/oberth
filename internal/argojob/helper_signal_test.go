package argojob

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The state root is the only injected input; holdEntry is the production
// subscription and dispatch used by RunHold, without an alternate signal path.
func TestReviewPublicEntrySignalChild(t *testing.T) {
	if os.Getenv("OBERTH_REVIEW_CHILD") != "1" {
		return
	}
	script := `trap 'printf caught > "$1"; exit "${OBERTH_REVIEW_EXIT:-0}"' USR1 USR2 TERM ALRM; printf ready; while :; do read line; done`
	if os.Getenv("OBERTH_REVIEW_ROUTINE") == "1" {
		script = `trap 'printf forwarded > "$1"; exit 0' CHLD URG WINCH PIPE; printf ready; read line; exit 7`
	}
	code := holdEntry(context.Background(), []string{"run", "--", "sh", "-c", script, "sh", os.Getenv("OBERTH_REVIEW_MARKER")}, HoldIO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}, os.Getenv("OBERTH_REVIEW_ROOT"))
	os.Exit(code)
}
func signalEntryProcess(t *testing.T, routine bool) (*exec.Cmd, io.WriteCloser, io.Reader, string, string) {
	t.Helper()
	root := holdTempDir(t)
	marker := filepath.Join(root, "caught")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestReviewPublicEntrySignalChild$")
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	mode := "0"
	if routine {
		mode = "1"
	}
	command.Env = append(os.Environ(), "OBERTH_REVIEW_CHILD=1", "OBERTH_REVIEW_ROUTINE="+mode, "OBERTH_REVIEW_ROOT="+root, "OBERTH_REVIEW_MARKER="+marker, "OBERTH_HOLD_ATTEMPT=0", "OBERTH_HOLD_LIMIT=0", "OBERTH_HOLD_TTL=1200", "OBERTH_HOLD_POD_UID=review-uid", "OBERTH_HOLD_PROTOCOL=2")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL) })
	ready := make([]byte, 5)
	if _, err = io.ReadFull(stdout, ready); err != nil || string(ready) != "ready" {
		t.Fatal("child did not start", err, string(ready))
	}
	return command, stdin, stderr, root, marker
}
func TestReviewPublicEntryForwardsSignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGALRM} {
		t.Run(sig.String(), func(t *testing.T) {
			command, _, _, _, marker := signalEntryProcess(t, false)
			if err := command.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			err := command.Wait()
			body, readErr := os.ReadFile(marker)
			if err != nil || readErr != nil || string(body) != "caught" {
				t.Fatalf("public helper did not forward %s: wait=%v marker=%q read=%v", sig, err, body, readErr)
			}
		})
	}
}
func TestPublicEntryRoutineSignalsDoNotCancel(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGCHLD, syscall.SIGURG, syscall.SIGWINCH, syscall.SIGPIPE} {
		t.Run(sig.String(), func(t *testing.T) { checkRoutineSignal(t, sig) })
	}
}
func checkRoutineSignal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	command, stdin, stderr, root, marker := signalEntryProcess(t, true)
	if err := command.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(stdin, "fail\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stderr).ReadString('\n')
	if err != nil || !strings.Contains(line, "command exited 7; held") {
		t.Fatal("routine signal canceled original or suppressed hold", sig, line, err)
	}
	_, next := sendHold(t, filepath.Join(root, "hold.sock"), holdRequest{Version: holdProtocol, UID: "review-uid", Op: "status"})
	if f := finalHold(t, next); f.Type != "held" {
		t.Fatal("routine signal closed hold", sig, f)
	}
	_, next = sendHold(t, filepath.Join(root, "hold.sock"), holdRequest{Version: holdProtocol, UID: "review-uid", Op: "release"})
	if f := finalHold(t, next); f.Type != "closed" {
		t.Fatal(f)
	}
	if err = command.Wait(); commandStatus(err) != 7 {
		t.Fatal("routine signal changed original result", sig, err)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("routine signal reached original command", sig, err)
	}
}

func TestPublicNonRoutineSignalSuppressesHold(t *testing.T) {
	t.Setenv("OBERTH_REVIEW_EXIT", "7")
	command, _, _, root, marker := signalEntryProcess(t, false)
	if err := command.Process.Signal(syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); commandStatus(err) != 7 {
		t.Fatal("non-routine signal entered a hold", err)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "caught" {
		t.Fatal("non-routine signal not forwarded", string(body), err)
	}
	if _, err := os.Stat(filepath.Join(root, "hold.sock")); !os.IsNotExist(err) {
		t.Fatal("canceled original held", err)
	}
}
