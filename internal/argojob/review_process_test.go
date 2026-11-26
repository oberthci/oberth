package argojob

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"k8s.io/utils/clock"
)

func reviewAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func reviewPids(t *testing.T, dir string) []int {
	t.Helper()
	var out []int
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			out = append(out, pid)
		}
	}
	return out
}

func reviewAllDead(t *testing.T, pids []int) {
	t.Helper()
	deadline := (clock.RealClock{}).Now().Add(3 * time.Second)
	for _, pid := range pids {
		for reviewAlive(pid) {
			if (clock.RealClock{}).Now().After(deadline) {
				t.Fatalf("pid %d survived close", pid)
			}
			reviewPause(10 * time.Millisecond)
		}
	}
}

// Race 1: many execs racing one release over the real socket.
func TestReviewRaceReleaseVersusManyExecs(t *testing.T) {
	t.Parallel()
	for round := 0; round < 5; round++ {
		checkReviewReleaseRound(t, round)
	}
}
func checkReviewReleaseRound(t *testing.T, round int) {
	o, path, _ := testHoldOwner(t)
	marks := t.TempDir()
	var wg sync.WaitGroup
	finals := make(chan string, 16)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			finals <- reviewExchange(path, []string{"sh", "-c", "echo $$ > \"$1\"; exec sleep 30", "sh", filepath.Join(marks, strconv.Itoa(i))})
		}(i)
	}
	reviewPause(time.Duration(round) * 5 * time.Millisecond)
	_, next := sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "release"})
	if f := finalHold(t, next); f.Type != "closed" {
		t.Fatal("release", f)
	}
	wg.Wait()
	close(finals)
	for f := range finals {
		if f != "closed" && f != "eof" {
			t.Fatal("exec ended with", f)
		}
	}
	// Anything that started must be dead; nothing may start afterwards.
	reviewAllDead(t, reviewPids(t, marks))
	before := len(reviewPids(t, marks))
	_, next = sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", Argv: []string{"sh", "-c", "echo $$ > \"$1\"", "sh", filepath.Join(marks, "late")}})
	if f := finalHold(t, next); f.Type != "closed" {
		t.Fatal("late exec", f)
	}
	reviewPause(50 * time.Millisecond)
	if len(reviewPids(t, marks)) != before {
		t.Fatal("exec started after release")
	}
	o.mu.Lock()
	_ = o.closing
	o.mu.Unlock()
}

// Race 2: expiry timer and SIGTERM racing running execs inside the real run path.
func TestReviewRaceExpiryAndSignalDuringExec(t *testing.T) {
	t.Parallel()
	for _, viaSignal := range []bool{false, true} {
		checkReviewExpiryRound(t, viaSignal)
	}
}
func checkReviewExpiryRound(t *testing.T, viaSignal bool) {
	r, testClock := testHoldRuntime(t)
	signals := make(chan os.Signal, 8)
	r.signals = signals
	diagnostic := &holdReadyWriter{ready: make(chan struct{})}
	done := make(chan int, 1)
	go func() {
		done <- r.dispatch(t.Context(), []string{"run", "--", "sh", "-c", "exit 7"}, HoldIO{In: strings.NewReader(""), Out: io.Discard, Err: diagnostic})
	}()
	ttl := <-testClock.timers
	<-diagnostic.ready
	marks := t.TempDir()
	sock := filepath.Join(r.root, "hold.sock")
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = reviewExchange(sock, []string{"sh", "-c", "echo $$ > \"$1\"; exec cat /dev/zero", "sh", filepath.Join(marks, strconv.Itoa(i))})
		}(i)
	}
	// Drain request timers so admissions never block on the test clock.
	go func() {
		for range testClock.timers {
		}
	}()
	reviewPause(30 * time.Millisecond)
	if viaSignal {
		signals <- syscall.SIGTERM
	} else {
		ttl <- testClock.Now()
	}
	select {
	case code := <-done:
		if code != 7 {
			t.Fatal("saved status not returned", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close waited for execs or drains")
	}
	wg.Wait()
	reviewAllDead(t, reviewPids(t, marks))
}

// Race 3: release and status finish while an admitted child's output is blocked.
func TestReviewRaceSilentReaderVersusRelease(t *testing.T) {
	o, path, child := reviewSilentInspection(t)
	statuses := make(chan string, 4)
	for range 4 {
		go func() { statuses <- reviewHoldStatus(path) }()
	}
	_, next := sendHold(t, path, holdRequest{Version: holdProtocol, UID: "uid", Op: "release"})
	if f := finalHold(t, next); f.Type != "closed" {
		t.Fatal(f)
	}
	// Receiving the reply does not order the server's subsequent close(done).
	<-o.done
	<-child.done
	if commandStatus(child.err) != 128+int(syscall.SIGKILL) {
		t.Fatal("release did not kill the blocked child", child.err)
	}
	for range 4 {
		if status := <-statuses; status != "held" && status != "closed" {
			t.Fatal("concurrent status", status)
		}
	}
}

// Disable the stream deadline so only closing its unread peer can unblock it.
type reviewSilentConn struct{ net.Conn }

func (reviewSilentConn) SetWriteDeadline(time.Time) error { return nil }

func reviewSilentInspection(t *testing.T) (*holdOwner, string, *inspection) {
	t.Helper()
	r, _ := testHoldRuntime(t)
	// Socket deadlines follow wall time; the test clock only owns hold timers.
	r.now = clock.RealClock{}.Now
	started := make(chan *inspection, 1)
	r.start = func(argv []string) (*inspection, error) {
		child, err := startInspection(argv)
		if err == nil {
			t.Cleanup(func() {
				select {
				case <-child.done:
				default:
					_ = syscall.Kill(-child.cmd.Process.Pid, syscall.SIGKILL)
				}
				<-child.done
			})
		}
		started <- child
		return child, err
	}
	o, path := startTestHoldOwner(t, r)
	server, client := net.Pipe()
	handled := make(chan struct{})
	t.Cleanup(func() { _ = client.Close(); _ = server.Close(); <-handled })
	observed := &observedHoldConn{Conn: reviewSilentConn{server}, writing: make(chan struct{})}
	go func() { o.request(observed); close(handled) }()
	if err := writeHoldFrame(client, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", Argv: []string{"cat", "/dev/zero"}}); err != nil {
		t.Fatal(err)
	}
	child := <-started
	if child == nil {
		t.Fatal("silent reader child did not start")
	}
	<-observed.writing
	return o, path, child
}

func reviewHoldStatus(path string) string {
	c, err := net.Dial("unix", path)
	if err != nil {
		return err.Error()
	}
	defer c.Close()
	if err := writeHoldFrame(c, holdRequest{Version: holdProtocol, UID: "uid", Op: "status"}); err != nil {
		return err.Error()
	}
	var f holdFrame
	if err := readHoldFrame(c, &f); err != nil {
		return err.Error()
	}
	return f.Type
}

// Real helper process: does the release caller ever see "closed" before exit?
func TestReviewHelperProcessEntry(t *testing.T) {
	if os.Getenv("REVIEW_HOLD_HELPER") != "1" {
		t.Skip("helper process only")
	}
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	r := holdRuntime{root: os.Getenv("REVIEW_ROOT"), start: startInspection, env: os.Getenv, now: clock.RealClock{}.Now, after: time.After, signals: signals}
	var args []string
	_ = json.Unmarshal([]byte(os.Getenv("REVIEW_ARGS")), &args)
	os.Exit(r.dispatch(context.Background(), args, HoldIO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}))
}

func reviewStartHelper(t *testing.T) (*exec.Cmd, string) {
	t.Helper()
	root := holdTempDir(t)
	args, _ := json.Marshal([]string{"run", "--", "sh", "-c", "exit 7"})
	cmd := exec.Command(os.Args[0], "-test.run=^TestReviewHelperProcessEntry$", "-test.count=1")
	cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "REVIEW_HOLD_HELPER=1", "REVIEW_ROOT="+root, "REVIEW_ARGS="+string(args),
		"OBERTH_HOLD_ATTEMPT=0", "OBERTH_HOLD_LIMIT=0", "OBERTH_HOLD_TTL=60", "OBERTH_HOLD_POD_UID=uid",
		"OBERTH_HOLD_PROTOCOL=2")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(root, "hold.sock")
	for i := 0; ; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			break
		}
		if i > 500 {
			_ = cmd.Process.Kill()
			t.Fatal("helper never listened")
		}
		reviewPause(10 * time.Millisecond)
	}
	return cmd, sock
}

func TestReviewReleaseReplyBeforeExit(t *testing.T) {
	t.Parallel()
	lost := 0
	const rounds = 200
	for i := 0; i < rounds; i++ {
		cmd, sock := reviewStartHelper(t)
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		if err := writeHoldFrame(conn, holdRequest{Version: holdProtocol, UID: "uid", Op: "release"}); err != nil {
			t.Fatal(err)
		}
		var f holdFrame
		err = readHoldFrame(conn, &f)
		_ = conn.Close()
		werr := cmd.Wait()
		var ee *exec.ExitError
		if !errors.As(werr, &ee) || ee.ExitCode() != 7 {
			t.Fatal("helper exit", werr)
		}
		if err != nil || f.Type != "closed" {
			lost++
		}
	}
	t.Logf("release reply lost in %d of %d real-process releases", lost, rounds)
	if lost > 0 {
		t.Fatalf("release reply lost in %d of %d rounds", lost, rounds)
	}
}

func reviewPause(d time.Duration) { <-(clock.RealClock{}).After(d) }

func reviewExchange(path string, argv []string) string {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return "dial"
	}
	defer conn.Close()
	_ = writeHoldFrame(conn, holdRequest{Version: holdProtocol, UID: "uid", Op: "exec", Argv: argv})
	for {
		var f holdFrame
		if readHoldFrame(conn, &f) != nil {
			return "eof"
		}
		if f.Type != "out" && f.Type != "err" {
			return f.Type
		}
	}
}
