package argojob

import (
	"bytes"
	"encoding/binary"
	"io"
	"k8s.io/utils/clock"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type holdTestClock struct {
	mu        sync.Mutex
	now       time.Time
	timers    chan chan time.Time
	durations map[<-chan time.Time]time.Duration
}

func newHoldTestClock() *holdTestClock {
	wall := clock.RealClock{}.Now
	return &holdTestClock{now: wall(), timers: make(chan chan time.Time, 32), durations: map[<-chan time.Time]time.Duration{}}
}
func (c *holdTestClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *holdTestClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.mu.Lock()
	c.durations[ch] = d
	c.mu.Unlock()
	c.timers <- ch
	return ch
}
func (c *holdTestClock) advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }
func holdTempDir(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "hold-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}
func testHoldRuntime(t *testing.T) (holdRuntime, *holdTestClock) {
	t.Helper()
	clock := newHoldTestClock()
	root := holdTempDir(t)
	values := map[string]string{"OBERTH_HOLD_ATTEMPT": "0", "OBERTH_HOLD_LIMIT": "0", "OBERTH_HOLD_TTL": "1200", "OBERTH_HOLD_POD_UID": "uid", "OBERTH_HOLD_PROTOCOL": "2"}
	return holdRuntime{root: root, stageRoot: holdTempDir(t), start: startInspection, env: func(key string) string { return values[key] }, now: clock.Now, after: clock.After, signals: make(chan os.Signal, 8), rerunPeer: func(net.Conn) bool { return true }}, clock
}
func testHoldOwner(t *testing.T) (*holdOwner, string, *holdTestClock) {
	t.Helper()
	r, c := testHoldRuntime(t)
	o, path := startTestHoldOwner(t, r)
	return o, path, c
}
func startTestHoldOwner(t *testing.T, r holdRuntime) (*holdOwner, string) {
	t.Helper()
	config, err := r.configuration()
	if err != nil {
		t.Fatal(err)
	}
	since := r.now()
	o := &holdOwner{runtime: r, config: config, since: since, deadline: since.Add(config.ttl), children: map[int]bool{}, stopped: make(chan struct{}), done: make(chan struct{})}
	path := filepath.Join(r.root, "hold.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	go o.serve(listener)
	t.Cleanup(func() { o.close(); _ = listener.Close() })
	return o, path
}
func sendHold(t *testing.T, path string, r holdRequest) (net.Conn, func() holdFrame) {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err = writeHoldFrame(conn, r); err != nil {
		t.Fatal(err)
	}
	return conn, func() holdFrame {
		t.Helper()
		var f holdFrame
		if err := readHoldFrame(conn, &f); err != nil {
			t.Fatal(err)
		}
		return f
	}
}
func finalHold(t *testing.T, next func() holdFrame) holdFrame {
	t.Helper()
	for {
		f := next()
		if f.Type != "out" && f.Type != "err" {
			return f
		}
	}
}
func TestBExecStartCloseOrdering(t *testing.T) {
	t.Parallel()
	for _, closeFirst := range []bool{false, true} {
		o, _, _ := testHoldOwner(t)
		sentinel := filepath.Join(t.TempDir(), "started")
		if closeFirst {
			o.close()
		}
		child, frame, _, _ := o.admit(holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", Argv: []string{"sh", "-c", "echo started > \"$1\"; exec cat /dev/zero > /dev/null", "sh", sentinel}})
		cleanupInspection(t, child)
		if closeFirst {
			if child != nil || frame.Type != "closed" {
				t.Fatal("payload admitted after close")
			}
			continue
		}
		if child == nil {
			t.Fatal(frame)
		}
		o.mu.Lock()
		registered := o.children[child.cmd.Process.Pid]
		o.mu.Unlock()
		if !registered {
			t.Fatal("child returned without registration")
		}
		o.close()
		<-child.done
		err := child.err
		_ = child.out.Close()
		_ = child.detail.Close()
		if commandStatus(err) == 0 {
			t.Fatal("registered child survived close")
		}
	}
}
func TestBRemoteDisconnectWithoutOutput(t *testing.T) {
	o, path, clock := testHoldOwner(t)
	conn, _ := sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", TimeoutMS: 10, Argv: []string{"sh", "-c", "exec cat /dev/zero >/dev/null"}})
	timer := <-clock.timers
	_ = conn.Close()
	timer <- clock.Now()
	select {
	case <-o.done:
		t.Fatal("request canceled entire hold")
	default:
	}
	// A remote loss can leave the local socket open; the owner's timer still ends it.
	_, next := sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", TimeoutMS: 10, Argv: []string{"sh", "-c", "exec cat /dev/zero >/dev/null"}})
	timer = <-clock.timers
	timer <- clock.Now()
	if f := finalHold(t, next); f.Type != "timeout" {
		t.Fatal(f)
	}
}
func TestBExecProtocolBounds(t *testing.T) {
	_, path, _ := testHoldOwner(t)
	for _, r := range []holdRequest{{}, {Version: 1, Op: "status", UID: "uid"}, {Version: holdProtocol, Op: "unknown", UID: "uid"}, {Version: holdProtocol, Op: "status"}, {Version: holdProtocol, Op: "exec", UID: "uid", TimeoutMS: -1}, {Version: holdProtocol, Op: "exec", UID: "uid", TimeoutMS: 900001}, {Version: holdProtocol, Op: "exec", UID: "uid", Argv: make([]string, 257)}} {
		_, next := sendHold(t, path, r)
		if f := finalHold(t, next); f.Type != "refused" {
			t.Fatal(f)
		}
	}
	checkMalformedHoldFrames(t, path)
	conn, next := sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", Argv: []string{"sh", "-c", "printf stdout; printf stderr >&2; exit 7"}})
	_ = conn.(*net.UnixConn).CloseWrite()
	var output string
	for {
		f := next()
		if f.Type == "exit" {
			if f.Code != 7 {
				t.Fatal(f)
			}
			break
		}
		output += string(f.Data)
	}
	if !strings.Contains(output, "stdout") || !strings.Contains(output, "stderr") {
		t.Fatal(output)
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], maxHoldFrame+1)
	var decoded holdRequest
	if readHoldFrame(bytes.NewReader(size[:]), &decoded) == nil {
		t.Fatal("oversize frame accepted")
	}
}
func checkMalformedHoldFrames(t *testing.T, path string) {
	t.Helper()
	for _, body := range [][]byte{{0, 0, 0, 1, '{'}, {0, 0, 0, 4, '{'}} {
		conn, err := net.Dial("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write(body)
		_ = conn.(*net.UnixConn).CloseWrite()
		var f holdFrame
		if err = readHoldFrame(conn, &f); err != nil || f.Type != "refused" {
			t.Fatal(f, err)
		}
		_ = conn.Close()
	}
}

func TestBActionAdmissionAndClose(t *testing.T) {
	o, path, clock := testHoldOwner(t)
	marker := filepath.Join(t.TempDir(), "effect")
	for _, uid := range []string{"", "another-run", "replaced-uid"} {
		_, next := sendHold(t, path, holdRequest{Version: holdProtocol, UID: uid, Op: "exec", Argv: []string{"sh", "-c", "touch \"$1\"", "sh", marker}})
		if f := finalHold(t, next); f.Type != "refused" {
			t.Fatal(f)
		}
	}
	clock.advance(21 * time.Minute)
	_, next := sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", Argv: []string{"touch", marker}})
	if f := finalHold(t, next); f.Type != "closed" {
		t.Fatal(f)
	}
	o.close()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("refused request had an effect", err)
	}
}
func TestBInspectionSurface(t *testing.T) {
	_, path, _ := testHoldOwner(t)
	output := filepath.Join(t.TempDir(), "output")
	_, next := sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", Argv: []string{"sh", "-c", "printf changed > \"$1\"", "sh", output}})
	if f := finalHold(t, next); f.Type != "exit" || f.Code != 0 {
		t.Fatal(f)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "changed" {
		t.Fatal(string(data), err)
	}
	_, next = sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", Argv: []string{"printf", "arbitrary"}})
	var text string
	for {
		f := next()
		text += string(f.Data)
		if f.Type == "exit" {
			break
		}
	}
	if text != "arbitrary" {
		t.Fatal(text)
	}
}
func TestBUnexpectedWrapperRestart(t *testing.T) {
	r, clock := testHoldRuntime(t)
	marker := filepath.Join(r.root, "invocations")
	args := []string{"run", "--", "sh", "-c", "echo once >> \"$1\"; exit 7", "sh", marker}
	diagnostic := &holdReadyWriter{ready: make(chan struct{})}
	streams := HoldIO{In: strings.NewReader(""), Out: io.Discard, Err: diagnostic}
	result := make(chan int, 1)
	go func() { result <- r.dispatch(t.Context(), args, streams) }()
	timer := <-clock.timers
	<-diagnostic.ready
	timer <- clock.Now()
	if code := <-result; code != 7 {
		t.Fatal(code)
	}
	if code := r.dispatch(t.Context(), args, streams); code != 70 {
		t.Fatal(code)
	}
	body, err := os.ReadFile(marker)
	if err != nil || string(body) != "once\n" {
		t.Fatal(string(body), err)
	}
	select {
	case <-clock.timers:
		t.Fatal("restart made a new hold deadline")
	default:
	}
}

func TestBHelperInstallationAndStartup(t *testing.T) {
	r, _ := testHoldRuntime(t)
	streams := HoldIO{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}
	if code := r.dispatch(t.Context(), []string{"install", "--to", r.root}, streams); code != 0 {
		t.Fatal(code)
	}
	info, err := os.Stat(filepath.Join(r.root, "oberth-hold"))
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatal(info, err)
	}
	checkHoldStartupConfiguration(t, r, streams)
	checkHoldStartupState(t, streams)
	checkHoldStartupStreams(t, streams)
}
func checkHoldStartupConfiguration(t *testing.T, r holdRuntime, streams HoldIO) {
	t.Helper()
	for _, key := range []string{"OBERTH_HOLD_ATTEMPT", "OBERTH_HOLD_LIMIT", "OBERTH_HOLD_TTL", "OBERTH_HOLD_POD_UID", "OBERTH_HOLD_PROTOCOL"} {
		broken := r
		broken.env = func(k string) string {
			if k == key {
				return ""
			}
			return r.env(k)
		}
		if code := broken.dispatch(t.Context(), []string{"run", "--", "true"}, streams); code != 126 {
			t.Fatal(key, code)
		}
	}
}
func checkHoldStartupState(t *testing.T, streams HoldIO) {
	t.Helper()
	var err error
	for _, kind := range []string{"file", "symlink", "missing"} {
		broken, _ := testHoldRuntime(t)
		target := filepath.Join(broken.root, "state")
		switch kind {
		case "file":
			if err = os.WriteFile(target, nil, 0600); err != nil {
				t.Fatal(err)
			}
		case "symlink":
			if err = os.Symlink(broken.root, target); err != nil {
				t.Fatal(err)
			}
		}
		broken.root = target
		if code := broken.dispatch(t.Context(), []string{"run", "--", "true"}, streams); code != 125 {
			t.Fatal(kind, code)
		}
	}
}
func checkHoldStartupStreams(t *testing.T, streams HoldIO) {
	t.Helper()
	missing, _ := testHoldRuntime(t)
	if code := missing.dispatch(t.Context(), []string{"run", "--", "/missing/command"}, streams); code != 127 {
		t.Fatal(code)
	}
	script, _ := testHoldRuntime(t)
	var stdout bytes.Buffer
	streams.Out = &stdout
	if code := script.dispatch(t.Context(), []string{"run", "--", "sh", "-c", "printf script-result"}, streams); code != 0 || stdout.String() != "script-result" {
		t.Fatal(code, stdout.String())
	}
}

type holdReadyWriter struct {
	mu    sync.Mutex
	body  bytes.Buffer
	ready chan struct{}
	once  sync.Once
}

func (w *holdReadyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.body.Write(p)
	if strings.Contains(w.body.String(), "release with run_hold_abandon") {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

func TestBSerialHoldsAccumulate(t *testing.T) {
	clock := newHoldTestClock()
	start := clock.Now()
	for i := 0; i < 2; i++ {
		r, _ := testHoldRuntime(t)
		r.now, r.after = clock.Now, clock.After
		diagnostic := &holdReadyWriter{ready: make(chan struct{})}
		done := make(chan int, 1)
		go func() {
			done <- r.dispatch(t.Context(), []string{"run", "--", "sh", "-c", "exit 7"}, HoldIO{In: strings.NewReader(""), Out: io.Discard, Err: diagnostic})
		}()
		timer := <-clock.timers
		<-diagnostic.ready
		_, next := sendHold(t, filepath.Join(r.root, "hold.sock"), holdRequest{Version: holdProtocol, UID: "uid", Op: "status"})
		if f := finalHold(t, next); f.Type != "held" || f.TTL != 1200 || !f.HeldSince.Equal(clock.Now()) {
			t.Fatal(f)
		}
		clock.advance(20 * time.Minute)
		timer <- clock.Now()
		if code := <-done; code != 7 {
			t.Fatal(code)
		}
	}
	if clock.Now().Sub(start) != 40*time.Minute {
		t.Fatal("serial waits collapsed")
	}
}
func TestBHelperFailureBindAndSignals(t *testing.T) {
	r, _ := testHoldRuntime(t)
	signals := make(chan os.Signal, 8)
	r.signals = signals
	if err := os.WriteFile(filepath.Join(r.root, "hold.sock"), []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	if code := r.dispatch(t.Context(), []string{"run", "--", "sh", "-c", "exit 7"}, HoldIO{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}); code != 7 {
		t.Fatal(code)
	}
	earlier, _ := testHoldRuntime(t)
	originalEnv := earlier.env
	earlier.env = func(k string) string {
		if k == "OBERTH_HOLD_LIMIT" {
			return "2"
		}
		return originalEnv(k)
	}
	if code := earlier.dispatch(t.Context(), []string{"run", "--", "sh", "-c", "exit 7"}, HoldIO{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard}); code != 7 {
		t.Fatal(code)
	}
	if _, err := os.Stat(filepath.Join(earlier.root, "hold.sock")); !os.IsNotExist(err) {
		t.Fatal("earlier attempt held")
	}
}

func cleanupInspection(t *testing.T, child *inspection) {
	t.Helper()
	if child == nil {
		return
	}
	t.Cleanup(func() {
		select {
		case <-child.done:
		default:
			_ = syscall.Kill(-child.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
}
