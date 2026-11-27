package argojob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"k8s.io/utils/clock"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// HoldIO preserves the original command's streams; helper diagnostics use Err.
type HoldIO struct {
	In       io.Reader
	Out, Err io.Writer
}
type holdRuntime struct {
	start         func([]string) (*inspection, error)
	root          string
	env           func(string) string
	now           func() time.Time
	after         func(time.Duration) <-chan time.Time
	signals       <-chan os.Signal
	sourceRoot    string
	sourceInfo    os.FileInfo
	rerunPeer     func(net.Conn) bool
	command       []string
	streams       HoldIO
	pendingSignal os.Signal
}
type holdConfig struct {
	attempt, limit int64
	ttl            time.Duration
	uid            string
}

// RunHold is the executable entry for oberth-hold and internal-hold install.
func RunHold(ctx context.Context, args []string, streams HoldIO) int {
	return holdEntry(ctx, args, streams, holdState)
}
func holdEntry(ctx context.Context, args []string, streams HoldIO, root string) int {
	signals := make(chan os.Signal, 32)
	signal.Notify(signals)
	defer signal.Stop(signals)
	runtime := holdRuntime{root: root, start: startInspection, env: os.Getenv, now: clock.RealClock{}.Now, after: time.After, signals: signals, rerunPeer: serverRerunPeer}
	return runtime.dispatch(ctx, args, streams)
}
func (r holdRuntime) dispatch(ctx context.Context, args []string, streams HoldIO) int {
	if len(args) == 3 && args[0] == "install" && args[1] == "--to" {
		return installHold(args[2], streams.Err)
	}
	if len(args) >= 3 && args[0] == "run" && args[1] == "--" {
		return r.run(ctx, args[2:], streams)
	}
	return r.client(ctx, args, streams)
}
func installHold(to string, diagnostic io.Writer) int {
	err := InstallExecutable(filepath.Join(to, "oberth-hold"))
	if err != nil {
		return holdDiagnostic(diagnostic, 125, err)
	}
	return 0
}
func (r holdRuntime) configuration() (holdConfig, error) {
	var c holdConfig
	var err error
	c.attempt, err = strconv.ParseInt(r.env("OBERTH_HOLD_ATTEMPT"), 10, 64)
	if err != nil || c.attempt < 0 {
		return c, fmt.Errorf("invalid configuration")
	}
	c.limit, err = strconv.ParseInt(r.env("OBERTH_HOLD_LIMIT"), 10, 64)
	if err != nil || c.limit < 0 {
		return c, fmt.Errorf("invalid configuration")
	}
	ttl, err := strconv.ParseInt(r.env("OBERTH_HOLD_TTL"), 10, 64)
	c.uid = r.env("OBERTH_HOLD_POD_UID")
	if err != nil || ttl <= 0 || ttl > 14400 || c.uid == "" || r.env("OBERTH_HOLD_PROTOCOL") != "2" {
		return c, fmt.Errorf("invalid configuration")
	}
	c.ttl = time.Duration(ttl) * time.Second
	return c, nil
}
func (r holdRuntime) claim() error {
	info, err := os.Lstat(r.root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("state is not a directory")
	}
	f, err := os.OpenFile(filepath.Join(r.root, "entry"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		err = f.Close()
	}
	return err
}
func holdDiagnostic(w io.Writer, code int, err error) int {
	_, _ = fmt.Fprintln(w, "oberth-hold:", err)
	return code
}
func commandStatus(err error) int {
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
	return 127
}
func (r holdRuntime) run(ctx context.Context, argv []string, streams HoldIO) int {
	c, err := r.configuration()
	if err != nil {
		return holdDiagnostic(streams.Err, 126, err)
	}
	if err = r.claim(); os.IsExist(err) {
		return holdDiagnostic(streams.Err, 70, fmt.Errorf("restarted; command not rerun"))
	}
	if err != nil {
		return holdDiagnostic(streams.Err, 125, err)
	}
	// Capture the mounted checkout before either the authored command or an
	// inspection can replace it. A missing checkout disables only rerun.
	r.sourceInfo, _ = os.Lstat(r.checkoutRoot())
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = streams.In, streams.Out, streams.Err
	if err = cmd.Start(); err != nil {
		return holdDiagnostic(streams.Err, 127, err)
	}

	code, canceled := r.waitOriginal(ctx, cmd)
	if code == 0 || c.attempt != c.limit || canceled || r.canceled(ctx) {
		return code
	}
	r.command, r.streams = argv, streams
	return r.hold(ctx, c, code, streams.Err)
}
func routineHoldSignal(s os.Signal) bool {
	switch s {
	case syscall.SIGCHLD, syscall.SIGURG, syscall.SIGWINCH, syscall.SIGPIPE:
		return true
	}
	return false
}
func (r holdRuntime) canceled(ctx context.Context) bool {
	for {
		select {
		case s := <-r.signals:
			if !routineHoldSignal(s) {
				return true
			}
		case <-ctx.Done():
			return true
		default:
			return false
		}
	}
}
func (r holdRuntime) waitOriginal(ctx context.Context, cmd *exec.Cmd) (int, bool) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	canceled := false
	canceledContext := ctx.Done()
	for {
		select {
		case err := <-done:
			return commandStatus(err), canceled
		case s := <-r.signals:
			if routineHoldSignal(s) {
				continue
			}
			canceled = true
			_ = cmd.Process.Signal(s)
		case <-canceledContext:
			canceledContext = nil
			canceled = true
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}
}
