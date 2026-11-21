//go:build linux

// These tests exercise the Linux server and Git receive path.

package gitcache_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

// rsSpec is what one server process of a random-stop run is told. Every stop
// is the whole server process tree ending at once (rsKiller); the next phase
// builds a new instance over the same root, as a restart would.
type rsSpec struct {
	Root, Scratch, Binary, Hook, Bare string
	Control                           string
	Mode                              string   // receive (one or two racing pushes) or recover
	Inputs                            []string // one pack stream per receive
	Refuse, AlwaysRun                 bool
	Transport                         string // clean, status-fail or input-cut
	// FaultOnly limits Refuse and Transport to one push (-1: every push);
	// Values holds each push's first new value, which names it at admission.
	FaultOnly int
	Values    []string
	Commit    string
	// Target is the ordinal on the main sequence (Git, hooks, server
	// callbacks) that stops the process, TransportTarget the one on the
	// transport sequence (pusher input, status output, diagnostics). The two
	// are counted apart because transport copies run beside Git; 0 never
	// matches.
	Target, TransportTarget int
}

// TestReceiveRandomStopWorker is the server process of one phase. It is a
// no-op unless the random-stop test starts it with a spec.
func TestReceiveRandomStopWorker(t *testing.T) {
	data := os.Getenv("OBERTH_RANDOM_STOP")
	if data == "" {
		return
	}
	var spec rsSpec
	if err := json.Unmarshal([]byte(data), &spec); err != nil {
		t.Fatal(err)
	}
	w := &rsWorker{t: t, spec: spec}
	rsWrite(t, filepath.Join(spec.Control, "server.pid"), strconv.Itoa(os.Getpid()))
	if spec.Mode == "recover" {
		w.arm()
	}
	c := rsOpen(t, w.config())
	var errs []string
	if spec.Mode == "recover" {
		errs = append(errs, rsText(c.ReplayPending(t.Context(), rsConsumer(spec.Control, "recover", w.event))))
	} else {
		w.arm()
		errs = w.receives(c)
	}
	result, err := json.Marshal(errs)
	if err != nil {
		t.Fatal(err)
	}
	rsWrite(t, filepath.Join(spec.Control, "result"), string(result))
}

type rsWorker struct {
	t    *testing.T
	spec rsSpec
}

func (w *rsWorker) faulted(push int) bool { return w.spec.FaultOnly < 0 || w.spec.FaultOnly == push }

func (w *rsWorker) arm() {
	rsWrite(w.t, filepath.Join(w.spec.Control, "target"), strconv.Itoa(w.spec.Target))
}

func (w *rsWorker) config() gitcache.Config {
	s := w.spec
	return gitcache.Config{
		Root: s.Root, Scratch: s.Scratch, Binary: s.Binary, HookBinary: s.Hook,
		Upstream: func(context.Context, string) (gitcache.Remote, error) {
			return gitcache.Remote{URL: s.Bare, DefaultBranch: "refs/heads/main", RepositoryID: 1}, nil
		},
		LiveOwners: func(context.Context, string) (gitcache.OwnerPage, error) { return gitcache.OwnerPage{}, nil },
		PreReceive: func(_ context.Context, e gitcache.ReceiveEvent, _ *gitcache.ReceiveSource) error {
			w.event("admission-enter")
			defer w.event("admission-return")
			if s.Refuse && w.faulted(slices.IndexFunc(s.Values, func(v string) bool {
				return len(e.Updates) > 0 && e.Updates[0].NewSHA == v
			})) {
				return errors.New("fixture admission refused")
			}
			return nil
		},
		Log:          func(reason, _ string) { w.event("log " + reason) },
		AbortReceive: func(string) { w.event("abort-receive") },
		RefChanged:   func(context.Context, string, []gitcache.RefUpdate) { w.event("ref-changed") },
	}
}

// receives runs the pushes at once. A second push starts once the first has
// begun its transport (or returned), so it arrives while the first holds the
// repository and the ordinals of a seed name the same points on every run.
func (w *rsWorker) receives(c *gitcache.Cache) []string {
	errs := make([]string, len(w.spec.Inputs))
	var wg sync.WaitGroup
	for i := range w.spec.Inputs {
		started := make(chan struct{})
		var once sync.Once
		begun := func() { once.Do(func() { close(started) }) }
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer begun()
			errs[i] = rsText(c.Receive(w.t.Context(), w.request(i, begun)))
		}()
		<-started
	}
	wg.Wait()
	return errs
}

func (w *rsWorker) request(i int, begun func()) gitcache.ReceiveRequest {
	data, err := os.ReadFile(w.spec.Inputs[i])
	if err != nil {
		w.t.Fatal(err)
	}
	tag := strconv.Itoa(i)
	diagnostics, output := w.create("diagnostics-"+tag), w.create("output-"+tag)
	input := &rsInput{data: data, event: w.event}
	transport := w.spec.Transport
	if !w.faulted(i) {
		transport = "clean"
	}
	if transport == "input-cut" {
		input.cut = len(data) / 2
	}
	return gitcache.ReceiveRequest{
		Repository: gittest.Repository, Actor: "agent", Fingerprint: "key", AlwaysRun: w.spec.AlwaysRun,
		Admission:   gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: w.spec.Commit},
		Input:       input,
		Output:      &rsOutput{seen: output, event: w.event, fail: transport == "status-fail"},
		Diagnostics: rsWriter{Writer: diagnostics, event: w.event},
		Begin:       func() { w.event("begin " + tag); begun() },
		End:         func() { w.event("end " + tag) },
		Deliver:     rsConsumer(w.spec.Control, "receive", w.event),
	}
}

func (w *rsWorker) create(name string) *os.File {
	f, err := os.Create(filepath.Join(w.spec.Control, name))
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = f.Close() })
	return f
}

// event claims the next ordinal of its sequence; the ordinal the test chose
// stops the whole server process tree right here.
func (w *rsWorker) event(what string) {
	stream, target := "", w.spec.Target
	if rsTransportEvent(what) {
		stream, target = rsTransportStream, w.spec.TransportTarget
	}
	n, err := rsClaim(w.spec.Control, stream, what)
	if err != nil {
		w.t.Error("event sequence:", err)
		return
	}
	if n != 0 && n == target {
		_ = exec.Command("/usr/bin/python3", filepath.Join(w.spec.Control, "killer.py"), w.spec.Control).Run()
		<-time.After(time.Minute)
		w.t.Error("server survived its stop at", what)
		os.Exit(3)
	}
}

// rsTransportStream prefixes the transport sequence's files and event lines.
const rsTransportStream = "t"

func rsTransportEvent(what string) bool {
	return strings.HasPrefix(what, "input-") || strings.HasPrefix(what, "output") || what == "diagnostics"
}

// rsClaim takes the next ordinal of a sequence under the flock the wrappers
// use for the main sequence.
func rsClaim(control, stream, what string) (int, error) {
	if _, err := os.Stat(filepath.Join(control, "target")); err != nil {
		return 0, nil
	}
	lock, err := os.OpenFile(filepath.Join(control, "seq.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return 0, err
	}
	defer func() { _ = lock.Close() }()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	seq := filepath.Join(control, stream+"seq")
	n, err := rsCount(seq)
	if err != nil {
		return 0, err
	}
	n++
	if err = os.WriteFile(seq, []byte(strconv.Itoa(n)+"\n"), 0600); err != nil {
		return 0, err
	}
	return n, rsAppend(filepath.Join(control, "events"), fmt.Sprintf("%s%d go %d %s\n", stream, n, os.Getpid(), what), false)
}

type rsInput struct {
	data       []byte
	offset     int
	cut        int
	started    bool
	event      func(string)
	closedOnce sync.Once
}

func (r *rsInput) Read(p []byte) (int, error) {
	if !r.started {
		r.started = true
		r.event("input-first-read")
	}
	if r.cut > 0 && r.offset >= r.cut {
		r.event("input-cut")
		return 0, errors.New("fixture pusher disconnected")
	}
	if r.offset >= len(r.data) {
		r.event("input-eof")
		return 0, io.EOF
	}
	end := len(r.data)
	if r.cut > 0 {
		end = r.cut
	}
	n := copy(p, r.data[r.offset:end])
	r.offset += n
	return n, nil
}

func (r *rsInput) Close() error {
	r.closedOnce.Do(func() { r.event("input-close") })
	return nil
}

// rsOutput is what the pusher reads; the file keeps only what reached it.
type rsOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	seen   io.Writer
	event  func(string)
	fail   bool
}

func (o *rsOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, _ = o.buffer.Write(p)
	if !bytes.Contains(o.buffer.Bytes(), []byte("unpack ")) {
		o.event("output")
		return o.seen.Write(p)
	}
	o.event("output-status")
	if o.fail {
		return 0, errors.New("fixture status write failed")
	}
	return o.seen.Write(p)
}

type rsWriter struct {
	io.Writer
	event func(string)
}

func (w rsWriter) Write(p []byte) (int, error) {
	w.event("diagnostics")
	return w.Writer.Write(p)
}

type rsDelivery struct {
	Phase   string               `json:"phase"`
	ID      string               `json:"id"`
	Updates []gitcache.RefUpdate `json:"updates"`
}

// rsConsumer is the durable consumer: it saves each event before it
// acknowledges it. "fail-once" makes it unavailable for exactly one call.
func rsConsumer(control, phase string, event func(string)) func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
	return func(_ context.Context, e gitcache.ReceiveEvent) ([]string, error) {
		event("deliver-enter")
		if os.Remove(filepath.Join(control, "fail-once")) == nil {
			return nil, errors.New("fixture consumer unavailable")
		}
		data, err := json.Marshal(rsDelivery{Phase: phase, ID: e.ID, Updates: e.Updates})
		if err != nil {
			return nil, err
		}
		if err = rsAppend(filepath.Join(control, "deliveries"), string(data)+"\n", true); err != nil {
			return nil, err
		}
		event("deliver-saved")
		return nil, nil
	}
}

func rsAppend(path, line string, sync bool) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(line)
	if err == nil && sync {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func rsCount(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func rsWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func rsText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// rsOpen builds a new instance over the root. The root lock is released only
// when every process of the stopped instance is gone, so it may need retries.
func rsOpen(t *testing.T, cfg gitcache.Config) *gitcache.Cache {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		c, err := gitcache.New(ctx, cfg)
		if err == nil {
			t.Cleanup(func() { _ = c.Close() })
			return c
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatal("new instance over a stopped root:", err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("stopped server still owns its root", err)
		case <-tick.C:
		}
	}
}

// rsEvent is the wrappers' copy of rsClaim plus the stop.
const rsEvent = `ev() {
	local n t
	exec 9>>"$R/seq.lock"
	/usr/bin/flock 9
	read -r n < "$R/seq"
	n=$((n + 1))
	echo "$n" > "$R/seq"
	echo "$n $1 $$ $2" >> "$R/events"
	exec 9>&-
	read -r t < "$R/target"
	if [ "$n" = "$t" ]; then
		/usr/bin/python3 "$R/killer.py" "$R"
		if [ ! -e "$R/survivors" ]; then exec /bin/sleep 60; fi
	fi
}
`

// rsGitWrapper counts every Git command before and after it runs. A native
// stop preloads the package's rename fault into the dynamic system Git.
func rsGitWrapper(control, binary string) string {
	return "#!/bin/bash\nR=" + shellQuote(control) + "\n" +
		"if [ ! -e \"$R/target\" ]; then exec " + shellQuote(binary) + " \"$@\"; fi\n" + rsEvent +
		"what=\"$*\"; what=\"${what#-c core.hooksPath=* }\"; what=\"${what:0:100}\"\n" +
		"ev git \"before $what\"\n" +
		"if [ -e \"$R/native\" ]; then\n" +
		"\tread -r needle when < \"$R/native\"\n" +
		"\tLD_PRELOAD=\"$R/fault.so\" REVIEW_NEEDLE=\"$needle\" REVIEW_PHASE=\"$when\" REVIEW_MARKER=\"$R/native-stop\" /usr/bin/git \"$@\"\n" +
		"else\n\t" + shellQuote(binary) + " \"$@\"\nfi\n" +
		"s=$?\nev git \"after $s $what\"\nexit $s\n"
}

// rsHookWrapper counts each hook invocation before and after it runs and
// applies the run's hook damage: a lost reference-transaction hook, an empty
// preparation, or a lost pre-receive hook.
func rsHookWrapper(control, binary string) string {
	run := "(exec -a \"$0\" " + shellQuote(binary) + " \"$@\")"
	return "#!/bin/bash\nR=" + shellQuote(control) + "\nexport GORACE=atexit_sleep_ms=0\n" +
		"if [ ! -e \"$R/target\" ]; then exec -a \"$0\" " + shellQuote(binary) + " \"$@\"; fi\n" + rsEvent +
		"name=\"${0##*/}\"\ndamage=none\n" +
		"if [ -e \"$R/damage\" ]; then read -r damage < \"$R/damage\"; fi\n" +
		"ev hook \"before $name $*\"\n" +
		"case \"$name/$damage/$1\" in\n" +
		"reference-transaction/rt-bypassed/*|pre-receive/pre-bypassed/) /bin/cat > /dev/null; s=0 ;;\n" +
		"reference-transaction/rt-empty/prepared) /bin/cat > /dev/null; " + run + " < /dev/null; s=$? ;;\n" +
		"*) " + run + "; s=$? ;;\nesac\n" +
		"ev hook \"after $s $name $*\"\nexit $s\n"
}

// rsKiller stops the server and then every descendant (Git, hooks, wrappers)
// before killing them all, so no part of the old instance runs past the stop.
// With "survivors" only the server dies, with what Pdeathsig takes along: Git
// is the server's direct child in production and one wrapper deeper here.
// Hooks then run on as orphans, as in the package's existing stop workers.
const rsKiller = `import os, signal, sys
R = sys.argv[1]
try:
    server = int(open(os.path.join(R, 'server.pid')).read())
    env = open('/proc/%d/environ' % server, 'rb').read()
except (OSError, ValueError):
    sys.exit(2)
if b'OBERTH_RANDOM_STOP=' not in env:
    sys.exit(2)
me = os.getpid()
def tree():
    kids, state = {}, {}
    for d in os.listdir('/proc'):
        if not d.isdigit():
            continue
        try:
            s = open('/proc/%s/stat' % d).read()
        except OSError:
            continue
        rest = s[s.rfind(')') + 2:].split()
        state[int(d)] = rest[0]
        kids.setdefault(int(rest[1]), []).append(int(d))
    out, todo = [], [(server, 0)]
    while todo:
        parent, depth = todo.pop()
        for c in kids.get(parent, []):
            if c != me:
                out.append((c, depth + 1, state[c]))
                todo.append((c, depth + 1))
    return out
def send(pids, sig):
    for p in pids:
        try:
            os.kill(p, sig)
        except OSError:
            pass
if os.path.exists(os.path.join(R, 'survivors')):
    send([server] + [p for p, depth, st in tree() if depth <= 2 or st == 'T'], signal.SIGKILL)
    sys.exit(0)
send([server], signal.SIGSTOP)
seen = []
while True:
    new = [p for p, _, _ in tree() if p not in seen]
    if not new:
        break
    send(new, signal.SIGSTOP)
    seen += new
send([server] + seen, signal.SIGKILL)
`
