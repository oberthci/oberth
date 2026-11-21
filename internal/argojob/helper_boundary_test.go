package argojob

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestBRequestBudgetKillsSilentProcess(t *testing.T) {
	o, path, clock := testHoldOwner(t)
	started := make(chan *inspection, 1)
	o.runtime.start = func(argv []string) (*inspection, error) {
		child, err := startInspection(argv)
		if err == nil {
			started <- child
		}
		return child, err
	}
	_, next := sendHold(t, path, holdRequest{Version: 1, UID: "uid", Op: "exec", TimeoutMS: 1, Argv: []string{"sh", "-c", "exec cat /dev/zero >/dev/null"}})
	child := <-started
	timer := <-clock.timers
	timer <- clock.Now()
	if f := finalHold(t, next); f.Type != "timeout" {
		t.Fatal(f)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	select {
	case <-child.done:
		if commandStatus(child.err) != 128+int(syscall.SIGKILL) {
			t.Fatal(child.err)
		}
	case <-ctx.Done():
		t.Fatal("request budget did not kill the silent process")
	}
}

type observedHoldConn struct {
	net.Conn
	writing chan struct{}
	once    sync.Once
}

func (c *observedHoldConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.writing) })
	return c.Conn.Write(p)
}
func TestBBlockedReaderCannotDelayClose(t *testing.T) {
	t.Parallel()
	o, _, _ := testHoldOwner(t)
	owner, client := net.Pipe()
	defer client.Close()
	observed := &observedHoldConn{Conn: owner, writing: make(chan struct{})}
	go o.request(observed)
	if err := writeHoldFrame(client, holdRequest{Version: 1, UID: "uid", Op: "exec", Argv: []string{"printf", "output"}}); err != nil {
		t.Fatal(err)
	}
	<-observed.writing
	closed := make(chan struct{})
	go func() { o.close(); close(closed) }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("close waited for a blocked reader")
	}
}
func TestBConfigAndEntryShapes(t *testing.T) {
	streams := HoldIO{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}
	for _, kind := range []string{"file", "directory", "symlink"} {
		r, _ := testHoldRuntime(t)
		entry := filepath.Join(r.root, "entry")
		var err error
		switch kind {
		case "file":
			err = os.WriteFile(entry, nil, 0600)
		case "directory":
			err = os.Mkdir(entry, 0700)
		case "symlink":
			err = os.Symlink("missing", entry)
		}
		if err != nil {
			t.Fatal(err)
		}
		if code := r.dispatch(t.Context(), []string{"run", "--", "true"}, streams); code != 70 {
			t.Fatal(kind, code)
		}
	}
	denied, _ := testHoldRuntime(t)
	if err := os.Chmod(denied.root, 0500); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(denied.root, 0700); err != nil {
			t.Error(err)
		}
	}()
	if code := denied.dispatch(t.Context(), []string{"run", "--", "true"}, streams); code != 125 {
		t.Fatal("denied claim", code)
	}
}
func TestBOriginalSignalIsForwardedWithoutHold(t *testing.T) {
	r, _ := testHoldRuntime(t)
	signals := make(chan os.Signal, 1)
	r.signals = signals
	output, writer := io.Pipe()
	defer output.Close()
	defer writer.Close()
	result := make(chan int, 1)
	go func() {
		result <- r.dispatch(t.Context(), []string{"run", "--", "sh", "-c", "printf ready; exec cat /dev/zero >/dev/null"}, HoldIO{In: strings.NewReader(""), Out: writer, Err: io.Discard})
	}()
	ready := make([]byte, 5)
	if _, err := io.ReadFull(output, ready); err != nil {
		t.Fatal(err)
	}
	signals <- syscall.SIGTERM
	if code := <-result; code != 143 {
		t.Fatal(code)
	}
	if _, err := os.Stat(filepath.Join(r.root, "hold.sock")); !os.IsNotExist(err) {
		t.Fatal("canceled original entered a hold")
	}
}
func TestBDiagnosticUsesOnlyStderr(t *testing.T) {
	r, clock := testHoldRuntime(t)
	c, err := r.configuration()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	holdFailureDiagnostic(c, 7, clock.Now(), &stderr)
	if !strings.Contains(stderr.String(), "command exited 7") {
		t.Fatal("missing diagnostic", stderr.String())
	}
}
func TestBDiagnosticIOCannotExtendTTL(t *testing.T) {
	r, clock := testHoldRuntime(t)
	diagnostic, writer := io.Pipe()
	defer diagnostic.Close()
	defer writer.Close()
	result := make(chan int, 1)
	go func() {
		result <- r.dispatch(t.Context(), []string{"run", "--", "sh", "-c", "exit 7"}, HoldIO{In: strings.NewReader(""), Out: io.Discard, Err: writer})
	}()
	timer := <-clock.timers
	timer <- clock.Now()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	select {
	case code := <-result:
		if code != 7 {
			t.Fatal(code)
		}
	case <-ctx.Done():
		t.Fatal("diagnostic I/O extended the hold")
	}
}

func TestBFramingPreservesArbitraryOutputBytes(t *testing.T) {
	t.Parallel()
	o, _, _ := testHoldOwner(t)
	var stdout, stderr bytes.Buffer
	code := o.runtime.client(t.Context(), []string{"exec", "--pod-uid", "uid", "--", "sh", "-c", "printf '\\377'; printf 'detail\\n' >&2"}, HoldIO{Out: &stdout, Err: &stderr})
	if code != 0 || !bytes.Equal(stdout.Bytes(), []byte{255}) || !strings.HasPrefix(stderr.String(), "detail\noberth-hold: ") {
		t.Fatal(code, stdout.Bytes(), stderr.String())
	}
}

func TestBOversizedRequestNeverReachesStart(t *testing.T) {
	t.Parallel()
	o, path, _ := testHoldOwner(t)
	launches := make(chan struct{}, 1)
	o.runtime.start = func([]string) (*inspection, error) { launches <- struct{}{}; return nil, os.ErrInvalid }
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	wrote := make(chan struct{})
	go func() {
		_ = writeHoldFrame(conn, holdRequest{Version: 1, UID: "uid", Op: "exec", Argv: []string{"true", strings.Repeat("x", maxHoldFrame)}})
		close(wrote)
	}()
	var f holdFrame
	if err = readHoldFrame(conn, &f); err != nil {
		t.Fatal(err)
	}
	<-wrote
	if f.Type != "refused" {
		t.Fatal("oversized request reached admission", f)
	}
	select {
	case <-launches:
		t.Fatal("oversized request reached start")
	default:
	}
}
