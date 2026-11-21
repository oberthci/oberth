package argojob

import (
	"context"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestDeltaHoldDiagnosticChangesOutputWithoutExec(t *testing.T) {
	r, _ := testHoldRuntime(t)
	result := filepath.Join(r.root, "result")
	prior := r.env
	r.env = func(key string) string {
		if key == "OBERTH_HOLD_TERMINATION" {
			return result
		}
		return prior(key)
	}
	var inspections atomic.Int32
	r.start = func(argv []string) (*inspection, error) { inspections.Add(1); return startInspection(argv) }
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ended := make(chan int, 1)
	diagnostic := &holdReadyWriter{ready: make(chan struct{})}
	go func() {
		ended <- r.run(ctx, []string{"sh", "-c", `printf original > "$1"; exit 7`, "sh", result}, HoldIO{In: null, Out: null, Err: diagnostic})
	}()
	select {
	case <-diagnostic.ready:
	case code := <-ended:
		t.Fatalf("helper ended before hold: %d", code)
	case <-time.After(2 * time.Second):
		t.Fatal("no hold diagnostic")
	}
	output, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	releaseProbeHold(t, r, ended)
	if inspections.Load() != 0 {
		t.Fatal("probe ran inspection")
	}
	if string(output) != "original" {
		t.Fatalf("helper changed output without inspection: %q", output)
	}
	assertFailedArmedRunExcluded(t)

}

func TestDeltaOriginalDescendantWritesDuringHold(t *testing.T) {
	r, _ := testHoldRuntime(t)
	result := filepath.Join(r.root, "result")
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	var inspections atomic.Int32
	r.start = func(argv []string) (*inspection, error) { inspections.Add(1); return startInspection(argv) }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// This is an original-command descendant, never an inspection command.
	script := `printf original > "$1"; (while [ ! -S "$2/hold.sock" ]; do :; done; printf background > "$1"; printf done > "$2/done") >/dev/null 2>&1 & printf '%s' "$!" > "$2/pid"; exit 7`
	ended := make(chan int, 1)
	go func() {
		ended <- r.run(ctx, []string{"sh", "-c", script, "sh", result, r.root}, HoldIO{In: null, Out: null, Err: null})
	}()
	defer func() {
		if b, e := os.ReadFile(filepath.Join(r.root, "pid")); e == nil {
			if pid, e := strconv.Atoi(string(b)); e == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	limit := time.NewTimer(2 * time.Second)
	defer limit.Stop()
wait:
	for {
		select {
		case <-ticker.C:
			if _, err = os.Stat(filepath.Join(r.root, "done")); err == nil {
				break wait
			}
		case <-limit.C:
			t.Fatal("original descendant did not complete during hold")
		}
	}
	body, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	releaseProbeHold(t, r, ended)
	if inspections.Load() != 0 {
		t.Fatal("inspection started")
	}
	if string(body) != "background" {
		t.Fatalf("original-command descendant did not write during hold: %q", body)
	}
	assertFailedArmedRunExcluded(t)
}

func assertFailedArmedRunExcluded(t *testing.T) {
	t.Helper()
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "produce", HoldArmed: true}}}
	nodes := []stateport.Node{{NodeID: "produce", Path: "produce", Attempt: 1, Status: stepmodel.StepFailed, Exit: new(7)}}
	if !stateport.ArmedStepMayHaveHeld(plan, nodes) {
		t.Fatal("uninspected armed failure became ordinary evidence")
	}
}

func releaseProbeHold(t *testing.T, r holdRuntime, ended <-chan int) {
	t.Helper()
	if code := r.client(t.Context(), []string{"release", "--pod-uid", "uid"}, HoldIO{Out: io.Discard, Err: io.Discard}); code != 122 {
		t.Fatal("release failed", code)
	}
	select {
	case code := <-ended:
		if code != 7 {
			t.Fatal("saved status changed", code)
		}
	case <-time.After(time.Second):
		t.Fatal("helper did not exit")
	}
}
