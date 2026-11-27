package argojob

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHoldRerunForwardsSignal(t *testing.T) {
	t.Parallel()
	r := rerunRuntime(t)
	signals := make(chan os.Signal, 8)
	r.signals = signals
	holding := &holdReadyWriter{ready: make(chan struct{})}
	running := &holdReadyWriter{ready: make(chan struct{})}
	ended := make(chan int, 1)
	go func() {
		ended <- r.run(t.Context(), []string{"sh", "-c", `test -f "$1/FIXED" || exit 7; printf 'release with run_hold_abandon\n'; exec cat /dev/zero >/dev/null`, "sh", r.sourceRoot}, HoldIO{In: strings.NewReader(""), Out: running, Err: holding})
	}()
	select {
	case <-holding.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("hold did not start")
	}
	bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	if code := r.client(t.Context(), []string{"rerun", "--pod-uid", "uid", "--sha256", digest}, HoldIO{In: bytes.NewReader(bundle), Out: io.Discard, Err: io.Discard}); code != 0 {
		t.Fatal(code)
	}
	select {
	case <-running.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("rerun did not start")
	}
	signals <- syscall.SIGTERM
	select {
	case code := <-ended:
		if code != 143 {
			t.Fatal("signal not native", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("signal not forwarded")
	}
}

func TestRerunMonitorRetainsConcurrentSignal(t *testing.T) {
	t.Parallel()
	r := rerunRuntime(t)
	bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stage)
	o, _, _ := testHoldOwner(t)
	o.runtime.sourceRoot, o.runtime.sourceInfo, o.runtime.root = r.sourceRoot, r.sourceInfo, r.root
	o.runtime.stageRoot = r.stageRoot
	signals := make(chan os.Signal, 1)
	o.runtime.signals = signals
	_, frame, _, _ := o.admit(holdRequest{Version: holdProtocol, Op: "rerun", UID: "uid", Stage: stage, Digest: digest})
	if frame.Type != "rerunning" {
		t.Fatal(frame)
	}
	signals <- syscall.SIGTERM
	if got := o.monitor(t.Context(), make(chan time.Time)); got != syscall.SIGTERM {
		t.Fatal("signal lost at one-shot transition", got)
	}
}

func TestRerunStopsRegisteredInspectionGroups(t *testing.T) {
	t.Parallel()
	r := rerunRuntime(t)
	bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stage)
	o, _, _ := testHoldOwner(t)
	o.runtime.sourceRoot, o.runtime.sourceInfo, o.runtime.root = r.sourceRoot, r.sourceInfo, r.root
	o.runtime.stageRoot = r.stageRoot
	child, frame, _, _ := o.admit(holdRequest{Version: holdProtocol, Op: "exec", UID: "uid", Argv: []string{"sh", "-c", "exec cat /dev/zero >/dev/null"}})
	if child == nil {
		t.Fatal(frame)
	}
	defer child.out.Close()
	defer child.detail.Close()
	_, frame, _, _ = o.admit(holdRequest{Version: holdProtocol, Op: "rerun", UID: "uid", Stage: stage, Digest: digest})
	if frame.Type != "rerunning" {
		t.Fatal(frame)
	}
	select {
	case <-child.done:
		if commandStatus(child.err) != 137 {
			t.Fatal("inspection survived rerun", child.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("inspection not killed")
	}
	if _, err = os.Stat(filepath.Join(r.sourceRoot, "FIXED")); !os.IsNotExist(err) {
		t.Fatal("admission wrote checkout under mutex")
	}
}
